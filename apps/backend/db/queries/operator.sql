-- VTX-13 operator dashboard reads and audited DLQ mutation.

-- name: RunStateHistogram :many
SELECT state, count(*) AS run_count FROM task_runs GROUP BY state ORDER BY state;

-- name: CountDeferredIntents :one
SELECT count(*) FROM deferred_intents WHERE disposition = 'pending_deferred';

-- name: CountActiveRuns :one
SELECT count(*) FROM task_runs
WHERE state IN ('preparing', 'coding', 'verifying', 'qa', 'quality_gate', 'pr_draft');

-- name: ListRunsFiltered :many
SELECT r.run_id, t.jira_issue_key, r.state, r.attempt, r.commit_sha, r.pr_url,
    r.lease_owner, r.error_code, r.created_at, r.updated_at
FROM task_runs r
JOIN registry_tasks t ON t.id = r.task_id
WHERE (sqlc.narg('state')::text IS NULL OR r.state = sqlc.narg('state')::text)
  AND (sqlc.narg('issue_key')::text IS NULL OR t.jira_issue_key = sqlc.narg('issue_key')::text)
  AND (sqlc.narg('before_updated_at')::timestamptz IS NULL
       OR (r.updated_at, r.run_id) < (sqlc.narg('before_updated_at')::timestamptz, sqlc.narg('before_run_id')::uuid))
ORDER BY r.updated_at DESC, r.run_id DESC
LIMIT sqlc.arg('row_limit')::int;

-- name: GetOperatorRunDetail :one
SELECT r.run_id, t.jira_issue_key, r.state, r.attempt, r.commit_sha, r.pr_url,
    r.lease_owner, r.error_code, r.error_message, r.created_at, r.updated_at,
    w.path AS worktree_path, w.branch AS worktree_branch, w.base_sha, w.status AS worktree_status
FROM task_runs r
JOIN registry_tasks t ON t.id = r.task_id
LEFT JOIN worktrees w ON w.run_id = r.run_id
WHERE r.run_id = $1 LIMIT 1;

-- name: OutboxStatsByType :many
SELECT event_type,
    count(*) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL) AS pending,
    count(*) FILTER (WHERE published_at IS NULL AND dead_lettered_at IS NULL AND attempt > 0) AS retrying,
    count(*) FILTER (WHERE dead_lettered_at IS NOT NULL) AS dead_lettered,
    COALESCE((EXTRACT(EPOCH FROM (CURRENT_TIMESTAMP - min(created_at) FILTER (WHERE published_at IS NULL)))::bigint), 0::bigint)::bigint AS oldest_age_seconds
FROM outbox_events
GROUP BY event_type
ORDER BY event_type;

-- name: ListDeadLettered :many
SELECT outbox_id, event_type, dead_letter_reason, attempt, dead_lettered_at, redrive_count
FROM outbox_events
WHERE dead_lettered_at IS NOT NULL
  AND (sqlc.narg('after')::uuid IS NULL OR outbox_id > sqlc.narg('after')::uuid)
ORDER BY outbox_id
LIMIT sqlc.arg('row_limit')::int;

-- name: RedriveDeadLetter :execrows
UPDATE outbox_events
SET dead_lettered_at = NULL, dead_letter_reason = NULL,
    next_attempt_at = CURRENT_TIMESTAMP, claimed_by = NULL, claimed_until = NULL,
    redrive_count = redrive_count + 1
WHERE outbox_id = $1 AND dead_lettered_at IS NOT NULL AND published_at IS NULL;

-- name: UpsertWorkerHeartbeatOp :exec
INSERT INTO worker_heartbeats (
    worker_id, worker_kind, queue, started_at, last_beat_at, active_runs, metadata
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (worker_id, worker_kind) DO UPDATE SET
    queue = EXCLUDED.queue, last_beat_at = EXCLUDED.last_beat_at,
    active_runs = EXCLUDED.active_runs, metadata = EXCLUDED.metadata;

-- name: ListWorkerHeartbeats :many
SELECT * FROM worker_heartbeats ORDER BY worker_kind, worker_id;

-- name: ListSSEEventsAfter :many
SELECT o.outbox_id, o.event_type, o.payload, o.created_at
FROM outbox_events o
WHERE o.event_type IN (
    'task_run.created.v1', 'task_run.state_changed.v1',
    'task_run.intent_deferred.v1', 'pull_request.opened.v1'
)
  AND (
    sqlc.narg('after_id')::uuid IS NULL
    OR (o.created_at, o.outbox_id) > (
        SELECT created_at, outbox_id FROM outbox_events WHERE outbox_id = sqlc.narg('after_id')::uuid
    )
  )
  AND (sqlc.narg('run_id')::text IS NULL OR o.payload->>'run_id' = sqlc.narg('run_id')::text)
ORDER BY o.created_at, o.outbox_id
LIMIT sqlc.arg('row_limit')::int;

-- name: InsertOperatorAudit :one
INSERT INTO operator_audit_log (
    actor, operator_id, action, target_type, target_id, reason,
    correlation_id, request_id, outcome, detail
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING audit_id;
