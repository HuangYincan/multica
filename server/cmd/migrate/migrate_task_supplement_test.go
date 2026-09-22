package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTaskSupplementMigrationsUpDownUpInIsolatedSchema(t *testing.T) {
	base := openTestPool(t)
	schema := fmt.Sprintf("task_supplement_migration_%d_%d", time.Now().UnixNano(), rand.Uint32())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = base.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})

	config, err := pgxpool.ParseConfig(testDatabaseURL())
	if err != nil {
		t.Fatalf("parse database config: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open schema-scoped pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `CREATE TABLE agent_task_queue (id UUID PRIMARY KEY, status TEXT NOT NULL)`); err != nil {
		t.Fatalf("create base task table: %v", err)
	}

	upVersions := []string{
		"536_task_supplement",
		"537_task_supplement_request_index",
		"538_task_supplement_capability_index",
		"539_task_supplement_comment_index",
		"540_task_supplement_ordinal_index",
	}
	downVersions := []string{
		"540_task_supplement_ordinal_index",
		"539_task_supplement_comment_index",
		"538_task_supplement_capability_index",
		"537_task_supplement_request_index",
		"536_task_supplement",
	}
	run := func(direction string, versions []string) {
		t.Helper()
		if err := runMigrations(ctx, pool, runOptions{
			Direction:             direction,
			Files:                 realMigrationFiles(t, versions, direction),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(direction),
			Conditions:            conditionsForDirection(direction),
		}); err != nil {
			t.Fatalf("migrate %s: %v", direction, err)
		}
	}

	run("up", upVersions)
	var validIndexes int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relname = ANY($2::text[])
		  AND i.indisvalid
	`, schema, []string{
		"task_supplement_task_request_uidx",
		"task_supplement_capability_task_uidx",
		"task_supplement_comment_uidx",
		"task_supplement_task_ordinal_uidx",
	}).Scan(&validIndexes); err != nil {
		t.Fatalf("inspect indexes: %v", err)
	}
	if validIndexes != 4 {
		t.Fatalf("valid supplement indexes = %d, want 4", validIndexes)
	}

	const taskID = "0199a4e8-22ce-7b01-bba5-000000000001"
	if _, err := pool.Exec(ctx, `INSERT INTO agent_task_queue (id, status) VALUES ($1, 'running')`, taskID); err != nil {
		t.Fatalf("insert running task: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_supplement_capability (task_id, workspace_id, issue_id, capability)
		VALUES ($1, $2, $3, 'task-supplement-v1')
	`, taskID,
		"0199a4e8-22ce-7b01-bba5-000000000002",
		"0199a4e8-22ce-7b01-bba5-000000000003",
	); err != nil {
		t.Fatalf("insert negotiated capability: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_supplement (
			task_id, workspace_id, issue_id, comment_id, author_id, client_request_id, ordinal, status
		) VALUES ($1, $2, $3, $4, $5, $6, 1, 'pending')
	`, taskID,
		"0199a4e8-22ce-7b01-bba5-000000000002",
		"0199a4e8-22ce-7b01-bba5-000000000003",
		"0199a4e8-22ce-7b01-bba5-000000000004",
		"0199a4e8-22ce-7b01-bba5-000000000005",
		"0199a4e8-22ce-7b01-bba5-000000000006",
	); err != nil {
		t.Fatalf("insert pending supplement: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID); err != nil {
		t.Fatalf("exercise terminal trigger: %v", err)
	}
	var status, reason string
	if err := pool.QueryRow(ctx, `SELECT status, failure_reason FROM task_supplement WHERE task_id = $1`, taskID).Scan(&status, &reason); err != nil {
		t.Fatalf("read terminal receipt: %v", err)
	}
	if status != "failed" || reason != "turn_ended" {
		t.Fatalf("terminal receipt = %q/%q", status, reason)
	}

	run("down", downVersions)
	var tables int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = $1 AND table_name IN ('task_supplement', 'task_supplement_capability')
	`, schema).Scan(&tables); err != nil {
		t.Fatalf("inspect rollback: %v", err)
	}
	if tables != 0 {
		t.Fatalf("supplement tables after down = %d, want 0", tables)
	}
	// Prove an old application schema remains usable after rollback, then that
	// a forward deploy can recreate the feature without manual repair.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_task_queue (id, status) VALUES ($1, 'running')`,
		"0199a4e8-22ce-7b01-bba5-000000000007"); err != nil {
		t.Fatalf("old schema task insert after down: %v", err)
	}
	run("up", upVersions)
}
