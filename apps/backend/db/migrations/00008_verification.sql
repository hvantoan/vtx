-- +goose Up
-- +goose StatementBegin
-- VTX-8: one row per verification suite execution, keyed to the EXACT commit
-- SHA evaluated. Append-only: retries insert a new attempt; nothing updates a
-- terminal verdict — only 'running' rows may transition to a terminal status.
CREATE TABLE IF NOT EXISTS verification_runs (
    -- kind reconciled with vtx-6: baseline rows run on base_sha before the
    -- agent; verification rows run on the agent commit.
    kind TEXT NOT NULL DEFAULT 'verification' CHECK (kind IN ('baseline', 'verification')),
    verification_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    attempt     INT  NOT NULL,                    -- verification attempt (>=1), independent of agent attempt
    suite       TEXT NOT NULL,                    -- 'make-test-boundaries' | 'make-vet' | ...
    command     TEXT NOT NULL,                    -- exact argv rendered, e.g. 'make -C <path> test'
    subject_commit_sha  TEXT NOT NULL,            -- HEAD at suite start — FR-QA-003 anchor
    status      TEXT NOT NULL CHECK (status IN (
        'running','passed','failed','error','timeout','skipped','superseded')),
    exit_code   INT,
    duration_ms INT,
    log_ref     TEXT,                             -- artifact URI or 'inline'
    log_tail    TEXT,                             -- bounded tail (<=64 KiB) when log_ref='inline'
    log_digest  TEXT,                             -- sha256 hex of full captured log
    error_detail TEXT,                            -- spawn/classification detail, never raw env
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT verification_runs_attempt_unique UNIQUE (run_id, attempt, suite)
);
-- "Is the full suite set green at this SHA for this run?" — gate lookup.
CREATE INDEX IF NOT EXISTS idx_verification_runs_sha
    ON verification_runs (run_id, subject_commit_sha, suite);
-- Reconcile sweep: rows stuck 'running' whose worker/lease is gone.
CREATE INDEX IF NOT EXISTS idx_verification_runs_running
    ON verification_runs (status) WHERE status = 'running';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS verification_runs;
-- +goose StatementEnd
