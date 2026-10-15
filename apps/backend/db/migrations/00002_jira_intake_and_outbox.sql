-- +goose Up
-- +goose StatementBegin
-- VTX-2 / SPEC-AUTO-01: Jira webhook intake, deduplication, transactional outbox.
-- Correctness invariants:
--   * jira_webhook_inbox records TERMINAL outcomes only — a retryable failure
--     must never claim the dedup key (the handler returns 503 pre-insert).
--   * Every column in the composite unique index is NOT NULL — Postgres treats
--     NULL != NULL, so a nullable site column would silently disable dedup.

CREATE TABLE IF NOT EXISTS execution_intents (
    intent_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source VARCHAR(32) NOT NULL DEFAULT 'jira_webhook',
    issue_key VARCHAR(64) NOT NULL,
    target_status_id VARCHAR(32) NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS jira_webhook_inbox (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    jira_site_id VARCHAR(255) NOT NULL,
    issue_id VARCHAR(32) NOT NULL,
    issue_key VARCHAR(64) NOT NULL,
    changelog_id VARCHAR(32) NOT NULL,
    target_status_id VARCHAR(32) NOT NULL,
    outcome VARCHAR(32) NOT NULL
        CHECK (outcome IN ('accepted', 'ignored_stale', 'ignored_scope', 'invalid_transition')),
    payload_hash CHAR(64) NOT NULL,
    request_id VARCHAR(128),
    correlation_id VARCHAR(128),
    intent_id UUID REFERENCES execution_intents(intent_id),
    error_detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Atomic dedup: (jira_site_id, issue_id, changelog_id, target_status_id).
-- A re-entry to Ready to Dev carries a NEW changelog_id -> a NEW intent.
CREATE UNIQUE INDEX IF NOT EXISTS idx_jira_webhook_inbox_unique
    ON jira_webhook_inbox (jira_site_id, issue_id, changelog_id, target_status_id);

CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id UUID NOT NULL REFERENCES execution_intents(intent_id) ON DELETE CASCADE,
    event_type VARCHAR(64) NOT NULL,
    payload JSONB NOT NULL,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished
    ON outbox_events (created_at)
    WHERE published_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS jira_webhook_inbox;
DROP TABLE IF EXISTS execution_intents;
-- +goose StatementEnd
