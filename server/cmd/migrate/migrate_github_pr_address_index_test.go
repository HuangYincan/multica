package main

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGitHubPRAddressIndexMigrationUpDownUp(t *testing.T) {
	adminPool := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	schema := createScratchSchema(t, ctx, adminPool, "migrate_github_pr_address_")
	pool := openTestPoolWithSearchPath(t, schema)
	for _, statement := range []string{
		`CREATE TABLE github_pull_request (
			id UUID PRIMARY KEY,
			workspace_id UUID NOT NULL,
			installation_id BIGINT NOT NULL,
			repo_owner TEXT NOT NULL,
			repo_name TEXT NOT NULL,
			pr_number INTEGER NOT NULL,
			head_sha TEXT NOT NULL,
			state TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX github_pull_request_workspace_id_repo_owner_repo_name_pr_nu_key
			ON github_pull_request (workspace_id, repo_owner, repo_name, pr_number)`,
		`INSERT INTO github_pull_request (
			id, workspace_id, installation_id, repo_owner, repo_name, pr_number, head_sha, state
		)
		SELECT ('10000000-0000-0000-0000-' || lpad(n::text, 12, '0'))::uuid,
		       ('20000000-0000-0000-0000-' || lpad(n::text, 12, '0'))::uuid,
		       (n % 250) + 1, 'owner-' || (n % 100), 'repo-' || (n % 1000),
		       n, 'sha-' || n, 'open'
		FROM generate_series(1, 50000) AS n`,
		`ANALYZE github_pull_request`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("apply fixture statement: %v", err)
		}
	}

	const indexName = "idx_github_pull_request_installation_repo_pr"
	const query = `
		SELECT id, workspace_id, head_sha, state
		FROM github_pull_request
		WHERE installation_id = 243
		  AND repo_owner = 'owner-42'
		  AND repo_name = 'repo-242'
		  AND pr_number = 4242`
	before := explainAnalyze(t, ctx, pool, query)
	if strings.Contains(before, indexName) {
		t.Fatalf("before plan unexpectedly uses absent index: %s", before)
	}

	const version = "533_github_pr_installation_repo_pr_index"
	options := runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, []string{version}, "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection("up"),
	}
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatalf("apply GitHub PR address index migration: %v", err)
	}
	assertGitHubPRAddressIndex(t, ctx, pool, indexName)
	after := explainAnalyze(t, ctx, pool, query)
	if !strings.Contains(after, indexName) {
		t.Fatalf("after plan does not use %s: %s", indexName, after)
	}
	t.Logf("before plan:\n%s\nafter plan:\n%s", before, after)

	options.Direction = "down"
	options.Files = realMigrationFiles(t, []string{version}, "down")
	options.Hooks = hooksForDirection("down")
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatalf("roll back GitHub PR address index migration: %v", err)
	}
	assertIndexExists(t, pool, schema, indexName, false)
	assertMigrationVersionRecorded(t, ctx, pool, schema, version, false)

	options.Direction = "up"
	options.Files = realMigrationFiles(t, []string{version}, "up")
	options.Hooks = hooksForDirection("up")
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatalf("reapply GitHub PR address index migration: %v", err)
	}
	assertGitHubPRAddressIndex(t, ctx, pool, indexName)
}

func assertGitHubPRAddressIndex(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	indexName string,
) {
	t.Helper()
	var definition string
	var unique, valid, ready, nonPartial bool
	var keyAttributes, totalAttributes int
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_indexdef(indexrelid), indisunique, indisvalid, indisready,
		       indpred IS NULL, indnkeyatts, indnatts
		FROM pg_index
		WHERE indexrelid = $1::regclass
	`, indexName).Scan(
		&definition, &unique, &valid, &ready, &nonPartial, &keyAttributes, &totalAttributes,
	); err != nil {
		t.Fatalf("read GitHub PR address index: %v", err)
	}
	if unique || !valid || !ready || !nonPartial || keyAttributes != 4 || totalAttributes != 4 {
		t.Fatalf(
			"index flags unique=%v valid=%v ready=%v non-partial=%v keys=%d attributes=%d",
			unique, valid, ready, nonPartial, keyAttributes, totalAttributes,
		)
	}
	if !strings.Contains(definition, "USING btree (installation_id, repo_owner, repo_name, pr_number)") {
		t.Fatalf("index definition = %q", definition)
	}
}

func explainAnalyze(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) string {
	t.Helper()
	rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+query)
	if err != nil {
		t.Fatalf("explain query: %v", err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read explain rows: %v", err)
	}
	return strings.Join(lines, "\n")
}
