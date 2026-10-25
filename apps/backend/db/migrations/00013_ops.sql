-- +goose Up
-- +goose StatementBegin
-- VTX-11: retry classification + redrive audit. This lane owns only the
-- VTX-11 content of 00013; operator_audit_log (VTX-13) lives in 00014.

-- Queryable failure classification on the run (audit metadata also carries it,
-- but columns make ops queries cheap: "all runs failed with lease_lost").
ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS failure_class TEXT;
ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS retry_policy_version TEXT;

-- Redrive audit for outbox dead letters (FR-EVT-002 "audited redrive").
-- A redrive = clear dead_lettered_at/dead_letter_reason on the outbox row and
-- record who did it and why. VTX-13's operator_audit_log (00014) is the
-- operator-side complement written by the same redrive path.
CREATE TABLE IF NOT EXISTS delivery_redrives (
    redrive_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    outbox_event_id UUID NOT NULL,          -- no FK: retention must not cascade
    consumer_name VARCHAR(64) NOT NULL,
    actor VARCHAR(64) NOT NULL,             -- operator principal
    actor_type TEXT NOT NULL DEFAULT 'human',
    reason TEXT NOT NULL,
    correlation_id VARCHAR(128),
    redriven_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_delivery_redrives_event
    ON delivery_redrives (outbox_event_id);

-- Sweeper scan support: nonterminal runs whose lease expired.
CREATE INDEX IF NOT EXISTS idx_task_runs_recovery
    ON task_runs (lease_expires_at)
    WHERE state NOT IN ('completed', 'failed', 'cancelled');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_task_runs_recovery;
DROP TABLE IF EXISTS delivery_redrives;
ALTER TABLE task_runs DROP COLUMN IF EXISTS failure_class;
ALTER TABLE task_runs DROP COLUMN IF EXISTS retry_policy_version;
-- +goose StatementEnd
