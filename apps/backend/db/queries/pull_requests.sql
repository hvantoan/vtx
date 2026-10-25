-- VTX-12 draft pull-request persistence.

-- name: InsertPullRequest :one
INSERT INTO pull_requests (
    run_id, repo_owner, repo_name, pr_number, pr_url, draft,
    head_branch, head_sha, base_branch, status, github_pr_id,
    ci_status, opened_at, error_code, error_detail
) VALUES (
    $1, $2, $3, $4, $5, TRUE, $6, $7, $8, $9, $10, $11, $12, $13, $14
)
RETURNING *;

-- name: GetPullRequestByRun :one
SELECT * FROM pull_requests WHERE run_id = $1 LIMIT 1;

-- name: GetPullRequestByNumber :one
SELECT * FROM pull_requests
WHERE repo_owner = $1 AND repo_name = $2 AND pr_number = $3
LIMIT 1;

-- name: MarkPullRequestStatus :one
UPDATE pull_requests
SET status = $2, error_code = $3, error_detail = $4,
    closed_at = CASE WHEN $2 IN ('closed', 'merged') THEN CURRENT_TIMESTAMP ELSE closed_at END,
    updated_at = CURRENT_TIMESTAMP
WHERE run_id = $1
RETURNING *;

-- name: UpdatePullRequestSync :one
UPDATE pull_requests
SET status = $2, ci_status = $3, last_synced_at = CURRENT_TIMESTAMP,
    closed_at = CASE WHEN $2 IN ('closed', 'merged') THEN CURRENT_TIMESTAMP ELSE closed_at END,
    updated_at = CURRENT_TIMESTAMP
WHERE run_id = $1
RETURNING *;

-- name: SetRunPrURL :exec
UPDATE task_runs SET pr_url = $2, updated_at = CURRENT_TIMESTAMP WHERE run_id = $1;

-- name: GetPRRunContext :one
SELECT r.*, t.repo_url, t.jira_issue_key
FROM task_runs r
JOIN registry_tasks t ON t.id = r.task_id
WHERE r.run_id = $1
LIMIT 1;

-- name: GetGateVerdict :one
SELECT decision_id, run_id, subject_commit_sha, decision, policy_name,
    policy_version, conditions, evaluated_at
FROM gate_decisions
WHERE run_id = $1 AND subject_commit_sha = $2
ORDER BY evaluated_at DESC
LIMIT 1;

-- name: InsertPullRequestOpenedEvent :exec
INSERT INTO outbox_events (outbox_id, intent_id, event_type, payload)
VALUES ($1, $2, 'pull_request.opened.v1', $3);

-- name: ListPullRequestsByStatus :many
SELECT * FROM pull_requests
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('before')::timestamptz IS NULL OR created_at < sqlc.narg('before')::timestamptz)
ORDER BY created_at DESC, pr_id DESC
LIMIT sqlc.arg('row_limit')::int;
