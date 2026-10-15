-- +goose Up
-- +goose StatementBegin
-- VTX-2 / review P2-1: append-only audit of EVERY authenticated webhook
-- request. jira_webhook_inbox stays terminal-outcome-only for fully
-- identified events (NOT NULL dedup key); this table covers everything the
-- gateway let through — malformed bodies, non-status events, wrong-target
-- transitions — that cannot legally claim the composite key.
--
-- Invariants:
--   * NO unique constraint — receipts never dedup; redelivery count is data.
--   * Identity columns nullable — transport rejects have no issue identity.
--   * payload_hash hashes what was read (a >256 KiB body is hashed only up to
--     the read cutoff — the hash is diagnostic, not a completeness proof).
CREATE TABLE IF NOT EXISTS webhook_receipts (
    receipt_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    received_at  TIMESTAMPTZ NOT NULL,
    outcome      VARCHAR(32) NOT NULL
        CHECK (outcome IN (
            'accepted', 'duplicate', 'ignored_stale', 'ignored_scope',
            'invalid_transition', 'rejected', 'failed_retryable'
        )),
    reason_code  VARCHAR(64),
    request_id   VARCHAR(128),
    payload_hash CHAR(64),
    issue_key    VARCHAR(64),
    error_detail TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Retention + investigation queries both filter on outcome and received_at.
CREATE INDEX IF NOT EXISTS idx_webhook_receipts_retention
    ON webhook_receipts (outcome, received_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS webhook_receipts;
-- +goose StatementEnd
