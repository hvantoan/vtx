-- +goose Up
-- +goose StatementBegin
-- VTX-12: GitHub draft pull-request records. Creation is structurally draft-only.
CREATE TABLE IF NOT EXISTS pull_requests (
    pr_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL UNIQUE REFERENCES task_runs(run_id) ON DELETE CASCADE,
    repo_owner TEXT NOT NULL,
    repo_name TEXT NOT NULL,
    pr_number INT,
    pr_url TEXT,
    draft BOOLEAN NOT NULL DEFAULT TRUE CHECK (draft IS TRUE),
    head_branch TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('requested', 'open', 'closed', 'merged', 'failed')),
    github_pr_id BIGINT,
    ci_status TEXT CHECK (ci_status IS NULL OR ci_status IN ('none', 'pending', 'success', 'failure', 'error')),
    last_synced_at TIMESTAMPTZ,
    opened_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    error_code TEXT,
    error_detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT pull_requests_github_unique UNIQUE (repo_owner, repo_name, pr_number)
);
CREATE INDEX IF NOT EXISTS idx_pull_requests_status
    ON pull_requests (status) WHERE status IN ('requested', 'open');

ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS pr_url TEXT;
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS redrive_count INT NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE outbox_events DROP COLUMN IF EXISTS redrive_count;
ALTER TABLE task_runs DROP COLUMN IF EXISTS pr_url;
DROP INDEX IF EXISTS idx_pull_requests_status;
DROP TABLE IF EXISTS pull_requests;
-- +goose StatementEnd
