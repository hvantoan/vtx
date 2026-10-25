-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS worker_heartbeats (
    worker_id TEXT NOT NULL,
    worker_kind TEXT NOT NULL CHECK (worker_kind IN
        ('temporal','dispatcher','scheduler','agent_host','sweeper')),
    run_id UUID REFERENCES task_runs(run_id) ON DELETE SET NULL,
    queue TEXT,
    active_runs INT NOT NULL DEFAULT 0,
    last_beat_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    metadata JSONB,
    started_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (worker_id, worker_kind)
);
CREATE INDEX IF NOT EXISTS idx_worker_heartbeats_beat
    ON worker_heartbeats (last_beat_at);
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS worker_heartbeats;
