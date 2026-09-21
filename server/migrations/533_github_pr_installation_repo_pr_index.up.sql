-- Match the full pull-request address used by snapshot refreshes so the same
-- installation can find rows across workspaces without a wide index scan.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_github_pull_request_installation_repo_pr
    ON github_pull_request (installation_id, repo_owner, repo_name, pr_number);
