-- +goose Up
-- +goose StatementBegin
-- VTX-13: measured worker liveness and audit for non-run operator mutations.
CREATE TABLE IF NOT EXISTS worker_heartbeats (
    worker_id TEXT NOT NULL,
    worker_kind TEXT NOT NULL CHECK (worker_kind IN ('temporal', 'dispatcher', 'scheduler', 'agent_host')),
    queue TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    last_beat_at TIMESTAMPTZ NOT NULL,
    active_runs INT NOT NULL DEFAULT 0,
    metadata JSONB,
    PRIMARY KEY (worker_id, worker_kind)
);
CREATE INDEX IF NOT EXISTS idx_worker_heartbeats_beat ON worker_heartbeats (last_beat_at);

CREATE TABLE IF NOT EXISTS operator_audit_log (
    audit_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor TEXT NOT NULL,
    operator_id TEXT,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    reason TEXT,
    correlation_id VARCHAR(128),
    request_id VARCHAR(128),
    outcome TEXT NOT NULL CHECK (outcome IN ('applied', 'rejected', 'error')),
    detail JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_operator_audit_target
    ON operator_audit_log (target_type, target_id, created_at DESC);

ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS redrive_count INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_task_runs_state_updated ON task_runs (state, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_outbox_dead_lettered
    ON outbox_events (dead_lettered_at) WHERE dead_lettered_at IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_outbox_dead_lettered;
DROP INDEX IF EXISTS idx_task_runs_state_updated;
DROP INDEX IF EXISTS idx_operator_audit_target;
DROP TABLE IF EXISTS operator_audit_log;
DROP INDEX IF EXISTS idx_worker_heartbeats_beat;
DROP TABLE IF EXISTS worker_heartbeats;
-- redrive_count is owned by 00012 and intentionally retained.
-- +goose StatementEnd
