-- +goose Up
-- +goose StatementBegin
-- VTX-4 / AUTO-03: git worktree lifecycle + lease.
--
-- Additive only. VTX-3 remains the run/lease authority: this migration adds no
-- lease columns because task_runs.lease_* already exists (00004). What was
-- missing is a writer — VTX-4 supplies it via AcquireRunLease/RenewRunLease/
-- ReleaseRunLease queries, plus the durable worktree allocation record.

-- repo_url is the VTX-4 contract input (advise decision 6): the allocation
-- path clones/fetches the parent repo per run. Nullable because VTX-3 rows
-- predate the field; AllocateWorktree rejects a run whose task has NULL.
ALTER TABLE registry_tasks ADD COLUMN IF NOT EXISTS repo_url TEXT;

CREATE TABLE IF NOT EXISTS worktrees (
    worktree_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- One worktree row per run, forever. The UNIQUE is the idempotency key:
    -- a retried AllocateWorktree on the same run returns the existing row.
    run_id UUID NOT NULL UNIQUE REFERENCES task_runs(run_id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('allocated', 'released', 'quarantined')),
    -- Canonical absolute path under WORKTREE_ROOT: <root>/<run_id>. Never a
    -- symlink; the service resolves and re-checks before any git or fs op.
    path TEXT NOT NULL,
    -- Also written to task_runs.branch (VTX-3 column) on allocation.
    branch TEXT NOT NULL,
    repo_url TEXT NOT NULL,
    base_sha TEXT,
    quarantine_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    released_at TIMESTAMPTZ
);

-- At most ONE allocated worktree per task (mirrors idx_task_runs_active_per_task
-- for runs). released/quarantined rows keep history without colliding.
CREATE UNIQUE INDEX IF NOT EXISTS idx_worktrees_active_per_task
    ON worktrees (task_id)
    WHERE status = 'allocated';

-- Reconcile scan: allocated rows joined back to runs whose lease expired or
-- whose state went terminal.
CREATE INDEX IF NOT EXISTS idx_worktrees_allocated
    ON worktrees (status)
    WHERE status = 'allocated';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_worktrees_allocated;
DROP INDEX IF EXISTS idx_worktrees_active_per_task;
DROP TABLE IF EXISTS worktrees;
ALTER TABLE registry_tasks DROP COLUMN IF EXISTS repo_url;
-- +goose StatementEnd
