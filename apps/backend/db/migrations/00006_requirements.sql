-- +goose Up
-- +goose StatementBegin

-- 00006_requirements.sql — frozen requirement snapshot per run.
CREATE TABLE IF NOT EXISTS run_requirements (
    run_id UUID PRIMARY KEY REFERENCES task_runs(run_id) ON DELETE CASCADE,
    intent_id UUID NOT NULL REFERENCES execution_intents(intent_id),
    jira_site_id TEXT NOT NULL,
    issue_id TEXT NOT NULL,
    issue_key TEXT NOT NULL,
    requirement_version TEXT NOT NULL,          -- 'vtx-req/v1'
    requirement_hash TEXT NOT NULL,             -- sha256 of canonical packet
    summary TEXT NOT NULL,
    acceptance_criteria JSONB NOT NULL,         -- string[] normalized ACs
    issue_snapshot JSONB NOT NULL,              -- raw-ish fetched fields, secrets stripped
    fetched_at TIMESTAMPTZ NOT NULL,            -- Jira re-fetch time
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS run_requirements;
