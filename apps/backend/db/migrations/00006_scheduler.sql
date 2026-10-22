-- +goose Up
-- +goose StatementBegin
-- VTX-5 / AUTO-04: coding-slot scheduler.
--
-- Additive only. VTX-3 stays the run/lease authority and VTX-4 stays the
-- worktree authority; this migration adds a separate coding queue and a
-- durable slot ledger. No run-state columns are touched, no guard is
-- weakened: granting a slot is admission evidence only, the
-- preparing → coding transition keeps requiring its real executor (VTX-7).
--
-- Safety contract this schema encodes:
--   * At most 3 slots held system-wide (enforced in the granting
--     transaction under pg_advisory_xact_lock — a single number, not
--     per-project and not per-worker).
--   * A run holds at most one held slot (partial unique index).
--   * A queue row's current grant is at most one held slot (partial index).
--   * Slot release is guarded by (run_id, grant_id, held): a stale or
--     repeated confirmation can release nothing.

-- FIFO order inside one Jira priority. BIGSERIAL gives a total order that
-- survives restarts; the timestamp is kept for observability only.
CREATE SEQUENCE IF NOT EXISTS coding_queue_fifo_seq;

CREATE TABLE IF NOT EXISTS coding_queue (
    queue_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- One queue entry per run, forever: enqueue is idempotent on run_id.
    run_id UUID NOT NULL UNIQUE REFERENCES task_runs(run_id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    jira_site_id VARCHAR(255) NOT NULL,
    jira_issue_id VARCHAR(32) NOT NULL,
    jira_issue_key VARCHAR(64) NOT NULL,
    -- Lower rank = scheduled earlier. Stored so ordering survives a Jira
    -- outage: the last verified priority still orders the queue.
    priority_rank INT NOT NULL,
    priority_name TEXT,
    fifo_seq BIGINT NOT NULL DEFAULT nextval('coding_queue_fifo_seq'),
    status TEXT NOT NULL CHECK (status IN (
        'waiting', 'allocated', 'blocked_dependency',
        'blocked_unverified', 'cancelled', 'released'
    )),
    blocked_reason TEXT,
    -- The currently held grant. Cleared on release; kept NULL for every
    -- non-allocated status so the slot ↔ queue link has exactly one writer.
    grant_id UUID,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Admission order: priority first, FIFO inside a priority.
CREATE INDEX IF NOT EXISTS idx_coding_queue_admission
    ON coding_queue (status, priority_rank, fifo_seq)
    WHERE status = 'waiting';

-- Blocked entries are re-evaluated: dependency reasons can change on Jira.
CREATE INDEX IF NOT EXISTS idx_coding_queue_blocked
    ON coding_queue (fifo_seq)
    WHERE status IN ('blocked_dependency', 'blocked_unverified');

CREATE TABLE IF NOT EXISTS coding_slots (
    -- grant_id is the fencing token: VTX-7 presents it to prove the agent it
    -- is about to start owns the slot, and stop-confirmations present it so a
    -- stale session can never release a newer grant.
    grant_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    queue_id UUID NOT NULL REFERENCES coding_queue(queue_id) ON DELETE CASCADE,
    run_id UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    worker_owner TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('held', 'released')),
    -- Wall-clock heartbeat bookkeeping. Missing heartbeats mark suspected
    -- but NEVER release the slot: only a confirmed agent-stop does.
    heartbeat_at TIMESTAMPTZ,
    suspected BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    released_at TIMESTAMPTZ,
    release_reason TEXT
);

-- AC-02 invariant: a run can never hold two slots at once (I-slot-unique).
CREATE UNIQUE INDEX IF NOT EXISTS idx_coding_slots_held_per_run
    ON coding_slots (run_id)
    WHERE status = 'held';

-- Capacity scan: held rows only, the hot path of every grant.
CREATE INDEX IF NOT EXISTS idx_coding_slots_held
    ON coding_slots (status)
    WHERE status = 'held';

-- Scheduler audit trail. Separate from task_run_events: these events are not
-- run state transitions and must not mint run versions.
CREATE TABLE IF NOT EXISTS scheduler_events (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type TEXT NOT NULL, -- enqueued | granted | released | cancelled | blocked | suspected | unblocked
    run_id UUID REFERENCES task_runs(run_id) ON DELETE SET NULL,
    queue_id UUID,
    grant_id UUID,
    actor TEXT NOT NULL,
    reason TEXT,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_scheduler_events_run
    ON scheduler_events (run_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_scheduler_events_run;
DROP TABLE IF EXISTS scheduler_events;
DROP INDEX IF EXISTS idx_coding_slots_held;
DROP INDEX IF EXISTS idx_coding_slots_held_per_run;
DROP TABLE IF EXISTS coding_slots;
DROP INDEX IF EXISTS idx_coding_queue_blocked;
DROP INDEX IF EXISTS idx_coding_queue_admission;
DROP TABLE IF EXISTS coding_queue;
DROP SEQUENCE IF EXISTS coding_queue_fifo_seq;
-- +goose StatementEnd
