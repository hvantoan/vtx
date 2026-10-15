-- VTX-2: Jira webhook intake queries. All dedup writes run inside the single
-- pgx.Tx opened by the application service (queries.WithTx).

-- name: InsertJiraWebhookInbox :one
-- Returns the inserted row; ON CONFLICT DO NOTHING yields an empty result on
-- duplicate delivery (pgx.ErrNoRows) — that is the dedup signal.
INSERT INTO jira_webhook_inbox (
    jira_site_id, issue_id, issue_key, changelog_id, target_status_id,
    outcome, payload_hash, request_id, correlation_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
ON CONFLICT (jira_site_id, issue_id, changelog_id, target_status_id) DO NOTHING
RETURNING event_id, jira_site_id, issue_id, issue_key, changelog_id,
    target_status_id, outcome, payload_hash, request_id, correlation_id,
    intent_id, error_detail, created_at, updated_at;

-- name: GetJiraWebhookInboxByEventKey :one
SELECT event_id, jira_site_id, issue_id, issue_key, changelog_id,
    target_status_id, outcome, payload_hash, request_id, correlation_id,
    intent_id, error_detail, created_at, updated_at
FROM jira_webhook_inbox
WHERE jira_site_id = $1
  AND issue_id = $2
  AND changelog_id = $3
  AND target_status_id = $4
LIMIT 1;

-- name: InsertExecutionIntent :one
INSERT INTO execution_intents (
    source, issue_key, target_status_id, status
) VALUES (
    $1, $2, $3, 'pending'
)
RETURNING intent_id, source, issue_key, target_status_id, status,
    created_at, updated_at;

-- name: InsertOutboxEvent :one
INSERT INTO outbox_events (
    intent_id, event_type, payload
) VALUES (
    $1, $2, $3
)
RETURNING outbox_id, intent_id, event_type, payload, published_at, created_at;

-- name: UpdateJiraWebhookInboxOutcome :exec
-- Links the accepted inbox row to its intent and stamps the terminal outcome.
UPDATE jira_webhook_inbox
SET intent_id = $2,
    outcome = $3,
    updated_at = CURRENT_TIMESTAMP
WHERE event_id = $1;
