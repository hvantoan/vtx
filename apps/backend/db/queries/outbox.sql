-- VTX-3 outbox dispatcher queries (§0.E: exactly ONE consumer identity, N
-- replicas — SKIP LOCKED spreads work, `claimed_by` never hides work from a
-- sibling). `published_at` is the consumer-specific ACK: it is stamped by the
-- dispatcher AFTER the effect transaction commits, never inside it (TR-20).

-- name: ClaimOutboxEvents :many
-- Single statement claim+stamp: the SKIP LOCKED sub-select and the UPDATE
-- cannot drift apart, so two dispatcher replicas never claim the same event.
-- Expired claims (claimed_until <= now()) are reclaimable after a crash.
UPDATE outbox_events o
SET claimed_by = sqlc.arg('consumer')::text,
    claimed_until = now() + make_interval(secs => sqlc.arg('claim_ttl_seconds')::int)
WHERE o.outbox_id IN (
    SELECT outbox_id
    FROM outbox_events
    WHERE published_at IS NULL
      AND dead_lettered_at IS NULL
      AND event_type = ANY(sqlc.arg('event_types')::text[])
      AND (next_attempt_at IS NULL OR next_attempt_at <= now())
      AND (claimed_until IS NULL OR claimed_until <= now())
    ORDER BY created_at, outbox_id
    LIMIT sqlc.arg('batch_size')::int
    FOR UPDATE SKIP LOCKED
)
RETURNING o.outbox_id, o.intent_id, o.event_type, o.payload, o.attempt,
    o.created_at;

-- name: MarkOutboxPublished :execrows
-- Consumer-specific ACK. `published_at IS NULL` keeps replay idempotent: a
-- second ACK for the same event reports 0 rows.
UPDATE outbox_events
SET published_at = CURRENT_TIMESTAMP,
    claimed_by = NULL,
    claimed_until = NULL
WHERE outbox_id = ANY(sqlc.arg('outbox_ids')::uuid[])
  AND published_at IS NULL;

-- name: ScheduleOutboxRetry :execrows
-- Bounded retry: bump attempt, back off, release the claim so a sibling
-- replica can pick the event up again.
UPDATE outbox_events
SET attempt = attempt + 1,
    next_attempt_at = CURRENT_TIMESTAMP + make_interval(secs => sqlc.arg('backoff_seconds')::int),
    claimed_by = NULL,
    claimed_until = NULL
WHERE outbox_id = ANY(sqlc.arg('outbox_ids')::uuid[]);

-- name: QuarantineOutboxEvent :execrows
-- Dead letter: unsupported event_type, schema-invalid payload or exhausted
-- retries. The row is kept (audit) and excluded from claim until redrive.
UPDATE outbox_events
SET dead_lettered_at = CURRENT_TIMESTAMP,
    dead_letter_reason = sqlc.arg('reason')::text,
    claimed_by = NULL,
    claimed_until = NULL
WHERE outbox_id = sqlc.arg('outbox_id')::uuid
  AND published_at IS NULL;

-- name: RedriveOutboxEvent :execrows
-- Operator action: clear the DLQ marker and let the dispatcher try again.
UPDATE outbox_events
SET dead_lettered_at = NULL,
    dead_letter_reason = NULL,
    attempt = 0,
    next_attempt_at = NULL,
    claimed_by = NULL,
    claimed_until = NULL
WHERE outbox_id = sqlc.arg('outbox_id')::uuid
  AND published_at IS NULL;

-- name: CountOutboxLag :one
-- Lag of the consumer that reads `event_type` — count + age of the oldest
-- still-pending event. `task_run.*` stays pending until VTX-5 exists: that is
-- the intended signal, not a stuck queue.
SELECT count(*) AS pending_count,
    min(created_at)::timestamptz AS oldest_pending_at
FROM outbox_events
WHERE published_at IS NULL
  AND dead_lettered_at IS NULL
  AND event_type = sqlc.arg('event_type')::varchar;
