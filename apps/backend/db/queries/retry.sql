-- VTX-11 retry/DLQ queries. Sweep reads select by predicate; the CAS on
-- task_runs.version (UpdateTaskRunState) is what makes a two-replica sweep a
-- no-op for the loser — no claim column needed.

-- name: ListRetryWaitRunsDue :many
SELECT *
FROM task_runs
WHERE state = 'retry_wait'
  AND retry_due_at IS NOT NULL
  AND retry_due_at <= sqlc.arg('now')::timestamptz
ORDER BY retry_due_at
LIMIT sqlc.arg('row_limit')::int;

-- name: ListExpiredLeaseRunsForRecovery :many
-- Crash-recovery scan: nonterminal runs whose lease expired. Parked states
-- (retry_wait/blocked/queued) are filtered out by the caller — a parked run's
-- stale lease is an expected leftover, not a dead worker.
SELECT *
FROM task_runs
WHERE state NOT IN ('completed', 'failed', 'cancelled', 'queued', 'blocked', 'retry_wait')
  AND lease_expires_at IS NOT NULL
  AND lease_expires_at < sqlc.arg('now')::timestamptz
ORDER BY lease_expires_at
LIMIT sqlc.arg('row_limit')::int;

-- name: GetOutboxEvent :one
SELECT outbox_id, intent_id, event_type, payload, attempt,
    published_at, dead_lettered_at, dead_letter_reason, created_at
FROM outbox_events
WHERE outbox_id = $1
LIMIT 1;

-- name: RedriveDeadLetteredEvent :execrows
-- Audited redrive step 1: clear the DLQ marker so the dispatcher re-claims
-- the event. Guarded on dead_lettered_at — a second redrive is a 0-row no-op.
UPDATE outbox_events
SET dead_lettered_at = NULL,
    dead_letter_reason = NULL,
    attempt = 0,
    next_attempt_at = NULL,
    claimed_by = NULL,
    claimed_until = NULL
WHERE outbox_id = sqlc.arg('outbox_id')::uuid
  AND published_at IS NULL
  AND dead_lettered_at IS NOT NULL;

-- name: InsertDeliveryRedrive :one
-- Audited redrive step 2: who redrove, for which event, and why.
INSERT INTO delivery_redrives (
    outbox_event_id, consumer_name, actor, actor_type, reason, correlation_id
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING redrive_id, outbox_event_id, consumer_name, actor, actor_type,
    reason, correlation_id, redriven_at;

-- name: ClearExpiredRunLeaseForRetryRecovery :execrows
-- Called only AFTER locking the run and verifying expected_version.
-- Epoch increment fences the dead worker in the same recovery transaction.
UPDATE task_runs SET lease_owner = NULL, lease_expires_at = NULL,
    lease_epoch = lease_epoch + 1
WHERE run_id = $1 AND lease_owner IS NOT NULL
    AND lease_expires_at < CURRENT_TIMESTAMP
    AND state NOT IN ('completed','failed','cancelled','blocked','retry_wait','queued');
