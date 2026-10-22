-- VTX-4 worktree lifecycle queries. Mutations run inside the single pgx.Tx
-- owned by WorktreeStore so lease acquisition, worktree row, and run branch
-- commit or roll back together — same boundary discipline as the registry.

-- name: AcquireRunLease :one
-- Writes the VTX-3 lease columns (they had no writer before VTX-4). Called
-- only on a row already locked FOR UPDATE by the caller's tx. The epoch bump
-- invalidates every previously observed epoch — that is the fencing token
-- stale holders get rejected on.
UPDATE task_runs
SET lease_owner = $2,
    lease_epoch = lease_epoch + 1,
    lease_expires_at = $3,
    updated_at = CURRENT_TIMESTAMP
WHERE run_id = $1
  AND state NOT IN ('completed', 'failed', 'cancelled')
RETURNING lease_owner, lease_epoch, lease_expires_at;

-- name: RenewRunLease :one
-- Only the recorded owner holding the presented epoch may renew, and only
-- while the run is live — a terminal run's lease must never be extended.
-- 0 rows ⇒ caller treats it as a lost lease (stale epoch).
UPDATE task_runs
SET lease_expires_at = $3,
    updated_at = CURRENT_TIMESTAMP
WHERE run_id = $1
  AND lease_owner = $2
  AND lease_epoch = $4
  AND lease_expires_at > CURRENT_TIMESTAMP
  AND state NOT IN ('completed', 'failed', 'cancelled')
RETURNING lease_expires_at;

-- name: ReleaseRunLease :exec
-- Idempotent release: owner+epoch match required, so a stale owner can never
-- clear a fresher lease.
UPDATE task_runs
SET lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE run_id = $1
  AND lease_owner = $2
  AND lease_epoch = $3;

-- name: SetRunBranch :exec
-- The allocated branch is persisted on the VTX-3 run row — VTX-4 does not
-- duplicate it into a new run-level column.
UPDATE task_runs
SET branch = $2,
    updated_at = CURRENT_TIMESTAMP
WHERE run_id = $1;

-- name: InsertWorktree :one
-- The run_id UNIQUE is the same-run idempotency key. A race that loses the
-- INSERT then reads the winner's row via GetWorktreeByRun.
INSERT INTO worktrees (
    run_id, task_id, status, path, branch, repo_url, base_sha
) VALUES (
    $1, $2, 'allocated', $3, $4, $5, $6
)
RETURNING worktree_id, run_id, task_id, status, path, branch, repo_url,
    base_sha, quarantine_reason, created_at, updated_at, released_at;

-- name: GetWorktreeByRun :one
SELECT worktree_id, run_id, task_id, status, path, branch, repo_url,
    base_sha, quarantine_reason, created_at, updated_at, released_at
FROM worktrees
WHERE run_id = $1
LIMIT 1;

-- name: GetActiveWorktreeForTask :one
SELECT worktree_id, run_id, task_id, status, path, branch, repo_url,
    base_sha, quarantine_reason, created_at, updated_at, released_at
FROM worktrees
WHERE task_id = $1 AND status = 'allocated'
LIMIT 1;

-- name: MarkWorktreeStatus :one
-- Guarded transition: 0 rows means the worktree already moved on (a
-- reconciler raced a release) and the caller must re-read.
UPDATE worktrees
SET status = $2,
    quarantine_reason = $3,
    released_at = CASE WHEN $2 = 'released' THEN CURRENT_TIMESTAMP ELSE released_at END,
    updated_at = CURRENT_TIMESTAMP
WHERE worktree_id = $1 AND status = $4
RETURNING worktree_id, run_id, task_id, status, path, branch, repo_url,
    base_sha, quarantine_reason, created_at, updated_at, released_at;

-- name: ListAllocatedWorktrees :many
-- Reconcile read: every allocated row joined to its run so the caller can see
-- terminal runs and expired leases without a second query.
SELECT w.worktree_id, w.run_id, w.task_id, w.status, w.path, w.branch,
    w.repo_url, w.base_sha, w.quarantine_reason, w.created_at, w.updated_at,
    w.released_at,
    r.state AS run_state,
    r.lease_owner AS run_lease_owner,
    r.lease_epoch AS run_lease_epoch,
    r.lease_expires_at AS run_lease_expires_at
FROM worktrees w
JOIN task_runs r ON r.run_id = w.run_id
WHERE w.status = 'allocated'
ORDER BY w.created_at;

-- name: GetRunLeaseState :one
-- Lightweight lease read for fencing checks that do not need the whole row.
SELECT lease_owner, lease_epoch, lease_expires_at, state
FROM task_runs
WHERE run_id = $1;

-- name: GetRunTaskRepo :one
-- Allocation input: the run's task identity fields plus its repo_url (nullable
-- for tasks created before VTX-4 or before a repo was recorded).
SELECT t.id AS task_id, t.repo_url, t.jira_issue_key
FROM task_runs r
JOIN registry_tasks t ON t.id = r.task_id
WHERE r.run_id = $1;

-- name: SetRegistryTaskRepoURL :exec
-- Allocation-time repo binding: the scheduler passes repo_url and the first
-- allocation records it on the task. Later allocations reuse the stored value.
UPDATE registry_tasks
SET repo_url = $2,
    updated_at = CURRENT_TIMESTAMP
WHERE id = $1 AND repo_url IS NULL;
