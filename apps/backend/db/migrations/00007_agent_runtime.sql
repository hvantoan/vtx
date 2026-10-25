-- +goose Up
-- +goose StatementBegin
-- VTX-7: durable record of every agent host process spawned for a run.
-- One row per attempt; the UNIQUE is the idempotency key for spawn retries.
CREATE TABLE IF NOT EXISTS agent_sessions (
    session_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    attempt      INT  NOT NULL,
    agent_type   TEXT NOT NULL CHECK (agent_type IN ('pi', 'omp')),
    agent_version TEXT,
    protocol_version TEXT NOT NULL,          -- negotiated version, e.g. '1.1'
    -- FR-AGT-003: false = real CLI; true = simulation double. Evidence
    -- consumers (VTX-9/10) must reject true rows for real-agent acceptance.
    is_test_double BOOLEAN NOT NULL,
    status       TEXT NOT NULL CHECK (status IN (
        'starting','ready','running','succeeded','failed',
        'cancelled','crashed','killed','agent_unavailable')),
    worktree_path TEXT NOT NULL,
    pid          INT,
    exit_reason  TEXT,                        -- cancel reason or escalation step
    error_code   TEXT,                        -- e.g. '-32003' / classify() code
    error_message TEXT,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT agent_sessions_attempt_unique UNIQUE (run_id, attempt)
);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_run ON agent_sessions (run_id);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_active
    ON agent_sessions (status) WHERE status IN ('starting','ready','running');

-- VTX-7: append-only stream of task/event notifications (the run's agent log).
-- (session_id, seq) unique ⇒ replay/idempotent append on supervisor restart.
CREATE TABLE IF NOT EXISTS agent_events (
    event_id    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id  UUID NOT NULL REFERENCES agent_sessions(session_id) ON DELETE CASCADE,
    run_id      UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    seq         INT  NOT NULL,
    event_type  TEXT NOT NULL CHECK (event_type IN (
        'stdout','stderr','progress','status_change',
        'tool_call','file_diff','error','log')),
    payload     TEXT NOT NULL DEFAULT '',      -- capped at 64 KiB by contract
    data        JSONB,                          -- structured form (v1.1)
    event_ts    TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT agent_events_seq_unique UNIQUE (session_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_agent_events_run ON agent_events (run_id, event_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS agent_events;
DROP TABLE IF EXISTS agent_sessions;
-- +goose StatementEnd
