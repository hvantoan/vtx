-- name: LockAdmissionLedger :exec
SELECT pg_advisory_xact_lock(hashtext('vtx-admission'));

-- name: ListQueuedRunsForAdmission :many
SELECT r.* FROM task_runs r WHERE state = 'queued'
ORDER BY created_at, run_id LIMIT $1 FOR UPDATE SKIP LOCKED;

-- name: CountLiveAdmissionSlots :one
SELECT COUNT(*) FROM task_runs
WHERE state NOT IN ('completed','failed','cancelled')
AND (state IN ('preparing','coding')
 OR lease_expires_at > CURRENT_TIMESTAMP
 OR (state = 'blocked' AND error_code = 'lease_lost'));

-- name: ListExpiredLeaseRuns :many
SELECT r.* FROM task_runs r
WHERE state NOT IN ('completed','failed','cancelled')
AND lease_expires_at < CURRENT_TIMESTAMP
ORDER BY lease_expires_at, run_id LIMIT $1 FOR UPDATE SKIP LOCKED;

-- name: ListDueRetryRuns :many
SELECT r.* FROM task_runs r
WHERE state = 'retry_wait' AND retry_due_at <= CURRENT_TIMESTAMP
ORDER BY retry_due_at, run_id LIMIT $1 FOR UPDATE SKIP LOCKED;

-- name: ListPromotableTasks :many
SELECT t.* FROM registry_tasks t
WHERE EXISTS (SELECT 1 FROM deferred_intents d WHERE d.task_id = t.id AND disposition = 'pending_deferred')
AND NOT EXISTS (SELECT 1 FROM task_runs r WHERE r.task_id = t.id AND state NOT IN ('completed','failed','cancelled'))
ORDER BY t.created_at, t.id LIMIT $1 FOR UPDATE SKIP LOCKED;

-- name: ClaimDeferredForTask :one
UPDATE deferred_intents SET disposition = 'admitted'
WHERE intent_id = (
 SELECT d.intent_id FROM deferred_intents d
 WHERE d.task_id = $1 AND d.disposition = 'pending_deferred'
 ORDER BY d.received_at, d.intent_id LIMIT 1 FOR UPDATE SKIP LOCKED
) RETURNING *;

-- name: GetDeferredIntentOrigin :one
SELECT event_id, changelog_id, correlation_id FROM jira_webhook_inbox
WHERE intent_id = $1 ORDER BY created_at LIMIT 1;

-- name: UpsertWorkerHeartbeat :one
INSERT INTO worker_heartbeats (worker_id, worker_kind, run_id, queue, active_runs, metadata)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (worker_id, worker_kind) DO UPDATE
SET last_beat_at = CURRENT_TIMESTAMP, run_id = EXCLUDED.run_id,
 queue = EXCLUDED.queue, active_runs = EXCLUDED.active_runs, metadata = EXCLUDED.metadata
RETURNING worker_id;

-- name: SetSchedulerRunError :exec
UPDATE task_runs SET error_code = $2 WHERE run_id = $1;
