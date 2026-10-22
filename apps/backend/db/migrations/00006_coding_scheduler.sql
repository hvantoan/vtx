-- +goose Up
-- +goose StatementBegin
-- VTX-5 / AUTO-04: system-wide coding slot scheduler.
--
-- Additive only: no existing table/column/index changes. Postgres is the
-- single source of truth for the (max 3) coding slots — no in-memory worker
-- counters. Apply after a database backup per DoD; all objects are new.

-- Singleton ledger row. Locking it FOR UPDATE inside the grant transaction
-- serializes slot allocation across every scheduler/worker replica.
CREATE TABLE IF NOT EXISTS coding_slot_pool (
    pool_id SMALLINT PRIMARY KEY CHECK (pool_id = 1),
    max_slots INT NOT NULL CHECK (max_slots > 0)
);
INSERT INTO coding_slot_pool (pool_id, max_slots) VALUES (1, 3)
ON CONFLICT (pool_id) DO NOTHING;

-- Durable queue of runs competing for a coding slot. A run enters only after
-- worktree preparation produced evidence (state 'preparing' + allocated
-- worktree row); preparation itself never holds a slot.
CREATE TABLE IF NOT EXISTS coding_queue_entries (
    entry_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL UNIQUE REFERENCES task_runs(run_id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    -- Fixed FIFO tie-breaker within the same Jira priority. Never reused.
    fifo_seq BIGINT GENERATED ALWAYS AS IDENTITY,
    status TEXT NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting', 'granted', 'closed')),
    -- Last admission decision reason (dependency_open:..., jira_unverified,
    -- run_terminal). NULL + recheck_at NULL means "fresh, evaluate now".
    block_reason TEXT,
    recheck_at TIMESTAMPTZ,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_coding_queue_waiting
    ON coding_queue_entries (fifo_seq)
    WHERE status = 'waiting';

CREATE TABLE IF NOT EXISTS coding_slot_grants (
    grant_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slot_no INT NOT NULL CHECK (slot_no > 0),
    run_id UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    entry_id UUID NOT NULL REFERENCES coding_queue_entries(entry_id) ON DELETE CASCADE,
    -- Scheduler/worker identity that issued the grant.
    owner TEXT NOT NULL,
    -- Fencing token: per-run grant sequence. A stop-confirmation carrying an
    -- older epoch must never release a slot granted to a newer session.
    session_epoch BIGINT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('held', 'suspect', 'released')),
    -- Last heartbeat from the executing worker; NULL = none received yet.
    heartbeat_at TIMESTAMPTZ,
    release_reason TEXT,
    release_confirmed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- The slot invariant: at most one non-released grant per slot number. Together
-- with max_slots this caps the whole system at 3 concurrent coding slots.
CREATE UNIQUE INDEX IF NOT EXISTS idx_coding_slot_grants_active_slot
    ON coding_slot_grants (slot_no)
    WHERE status <> 'released';

-- AC-03: one run can never hold two active coding grants.
CREATE UNIQUE INDEX IF NOT EXISTS idx_coding_slot_grants_active_run
    ON coding_slot_grants (run_id)
    WHERE status <> 'released';

-- One task can never hold two active coding grants (belt on top of the
-- task_runs partial unique index that already forbids concurrent runs).
CREATE UNIQUE INDEX IF NOT EXISTS idx_coding_slot_grants_active_task
    ON coding_slot_grants (task_id)
    WHERE status <> 'released';

-- A queue entry yields at most one active grant (re-grant after release uses
-- the same entry; the unique keeps a second grant impossible anyway).
CREATE UNIQUE INDEX IF NOT EXISTS idx_coding_slot_grants_active_entry
    ON coding_slot_grants (entry_id)
    WHERE status <> 'released';

-- Append-only scheduler audit. Separate from task_run_events: slot lifecycle
-- events are not run-state transitions and have no run version.
CREATE TABLE IF NOT EXISTS coding_slot_events (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grant_id UUID REFERENCES coding_slot_grants(grant_id) ON DELETE SET NULL,
    entry_id UUID REFERENCES coding_queue_entries(entry_id) ON DELETE SET NULL,
    run_id UUID,
    action TEXT NOT NULL,
    actor TEXT NOT NULL,
    reason TEXT,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_coding_slot_events_run
    ON coding_slot_events (run_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS coding_slot_events;
DROP TABLE IF EXISTS coding_slot_grants;
DROP TABLE IF EXISTS coding_queue_entries;
DROP TABLE IF EXISTS coding_slot_pool;
-- +goose StatementEnd
