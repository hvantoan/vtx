-- +goose Up
-- +goose StatementBegin
-- VTX-3 / SPEC-AUTO-02: durable task registry + execution state machine.
--
-- Additive only. Decision D6/§1.1: the bootstrap tables (tasks, task_events,
-- agent_sessions) are PRESERVED — VTX-3 uses the new `registry_tasks` identity
-- table instead of reusing `tasks`, so no rename/copy/DROP is involved. The
-- VTX-2 tables (execution_intents, jira_webhook_inbox, webhook_receipts) are
-- untouched; `outbox_events` is extended in place.
--
-- ponytail: no extra index for the dispatcher claim beyond the VTX-2 partial
-- index on (created_at) WHERE published_at IS NULL. Add
-- (next_attempt_at, created_at) WHERE published_at IS NULL AND dead_lettered_at
-- IS NULL when the backlog is large enough to make the ordering hurt.

CREATE TABLE IF NOT EXISTS registry_tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    jira_site_id VARCHAR(255) NOT NULL,
    jira_issue_id VARCHAR(32) NOT NULL,
    jira_issue_key VARCHAR(64) NOT NULL,
    project_key VARCHAR(64) NOT NULL,
    version BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- Identity key: one registry task per Jira issue per site (spec §3).
    CONSTRAINT registry_tasks_identity_unique UNIQUE (jira_site_id, jira_issue_id)
);

CREATE TABLE IF NOT EXISTS task_runs (
    run_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    -- One run per intent: the intent is the causal origin, never shared (§0.C).
    intent_id UUID NOT NULL UNIQUE REFERENCES execution_intents(intent_id) ON DELETE CASCADE,
    correlation_id VARCHAR(128),
    -- Causal origin of the run; kept as a plain UUID (no FK) so webhook
    -- retention can never cascade into run history.
    origin_event_id UUID,
    changelog_id VARCHAR(32),
    state TEXT NOT NULL CHECK (state IN (
        'queued', 'preparing', 'coding', 'verifying', 'qa', 'quality_gate',
        'pr_draft', 'completed', 'blocked', 'retry_wait', 'failed', 'cancelled'
    )),
    version BIGINT NOT NULL DEFAULT 0,
    attempt INT NOT NULL DEFAULT 0,
    -- Only meaningful for the two nonterminal side states (§0.C).
    resume_state TEXT CHECK (resume_state IS NULL OR resume_state IN (
        'queued', 'preparing', 'coding', 'verifying', 'qa', 'quality_gate', 'pr_draft'
    )),
    retry_due_at TIMESTAMPTZ,
    commit_sha TEXT,
    branch TEXT,
    error_code TEXT,
    error_message TEXT,
    lease_owner TEXT,
    lease_epoch BIGINT NOT NULL DEFAULT 0,
    lease_expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);

-- I-03: at most ONE admitted (nonterminal) run per task. Partial index so
-- terminal rows never collide and history stays append-only.
CREATE UNIQUE INDEX IF NOT EXISTS idx_task_runs_active_per_task
    ON task_runs (task_id)
    WHERE state NOT IN ('completed', 'failed', 'cancelled');

CREATE TABLE IF NOT EXISTS task_run_events (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    -- '' on the version-0 creation row: a run is born, it does not come from a
    -- state. Never a value from the domain state alphabet.
    previous_state TEXT NOT NULL,
    current_state TEXT NOT NULL,
    action TEXT NOT NULL,
    actor TEXT NOT NULL,
    actor_type TEXT NOT NULL,
    correlation_id VARCHAR(128),
    -- Audit version: equals the run version AFTER the transition (I-05).
    version BIGINT NOT NULL,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT task_run_events_version_unique UNIQUE (run_id, version)
);

CREATE TABLE IF NOT EXISTS deferred_intents (
    intent_id UUID PRIMARY KEY REFERENCES execution_intents(intent_id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    received_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    disposition VARCHAR(32) NOT NULL
        CHECK (disposition IN ('pending_deferred', 'admitted', 'expired')),
    reason TEXT
);

-- FIFO wait queue: the order the admission path (VTX-5) must drain.
CREATE INDEX IF NOT EXISTS idx_deferred_intents_fifo
    ON deferred_intents (received_at, intent_id);

CREATE TABLE IF NOT EXISTS inbox_consumptions (
    consumer_name VARCHAR(64) NOT NULL,
    source_event_id UUID NOT NULL,
    run_id UUID REFERENCES task_runs(run_id) ON DELETE SET NULL,
    consumed_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- Receipt key: exactly-once persisted effect per (consumer, event).
    PRIMARY KEY (consumer_name, source_event_id)
);

-- Dispatcher claim/backoff/DLQ state. All columns are ADD-only: every VTX-2
-- column and index keeps its meaning (`published_at IS NULL` = not delivered).
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS claimed_by TEXT;
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS claimed_until TIMESTAMPTZ;
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS attempt INT NOT NULL DEFAULT 0;
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS dead_lettered_at TIMESTAMPTZ;
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS dead_letter_reason TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE outbox_events
    DROP COLUMN IF EXISTS claimed_by,
    DROP COLUMN IF EXISTS claimed_until,
    DROP COLUMN IF EXISTS attempt,
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS dead_lettered_at,
    DROP COLUMN IF EXISTS dead_letter_reason;

DROP TABLE IF EXISTS inbox_consumptions;
DROP TABLE IF EXISTS deferred_intents;
DROP TABLE IF EXISTS task_run_events;
DROP INDEX IF EXISTS idx_task_runs_active_per_task;
DROP TABLE IF EXISTS task_runs;
DROP TABLE IF EXISTS registry_tasks;
-- +goose StatementEnd
