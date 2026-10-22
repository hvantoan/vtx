-- VTX-5 scheduler queries. Every granting path serializes through
-- LockScheduler inside one transaction: the count of held slots and the
-- grant insert are never separated outside that protection.

-- name: LockScheduler :exec
-- Transaction-scoped advisory lock. One fixed key serializes every
-- admission decision across all scheduler/worker replicas on this database.
SELECT pg_advisory_xact_lock(420005);

-- name: InsertQueueEntry :one
-- run_id UNIQUE is the enqueue idempotency key.
INSERT INTO coding_queue (
    run_id, task_id, jira_site_id, jira_issue_id, jira_issue_key,
    priority_rank, priority_name
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
ON CONFLICT (run_id) DO NOTHING
RETURNING queue_id, run_id, task_id, jira_site_id, jira_issue_id,
    jira_issue_key, priority_rank, priority_name, fifo_seq, status,
    blocked_reason, grant_id, enqueued_at, updated_at;

-- name: GetQueueEntryByRun :one
SELECT queue_id, run_id, task_id, jira_site_id, jira_issue_id,
    jira_issue_key, priority_rank, priority_name, fifo_seq, status,
    blocked_reason, grant_id, enqueued_at, updated_at
FROM coding_queue
WHERE run_id = $1
LIMIT 1;

-- name: GetQueueEntryForUpdate :one
-- Locks the queue row inside the admission transaction.
SELECT queue_id, run_id, task_id, jira_site_id, jira_issue_id,
    jira_issue_key, priority_rank, priority_name, fifo_seq, status,
    blocked_reason, grant_id, enqueued_at, updated_at
FROM coding_queue
WHERE queue_id = $1
FOR UPDATE;

-- name: ListSchedulableEntries :many
-- waiting + blocked rows in evaluation order. The admission sort is applied
-- in Go (priority first, FIFO inside); the index only narrows the scan.
SELECT queue_id, run_id, task_id, jira_site_id, jira_issue_id,
    jira_issue_key, priority_rank, priority_name, fifo_seq, status,
    blocked_reason, grant_id, enqueued_at, updated_at
FROM coding_queue
WHERE status IN ('waiting', 'blocked_dependency', 'blocked_unverified')
ORDER BY fifo_seq;

-- name: UpdateQueueStatus :exec
UPDATE coding_queue
SET status = $2,
    blocked_reason = $3,
    updated_at = CURRENT_TIMESTAMP
WHERE queue_id = $1;

-- name: SetQueuePriority :exec
-- Priority refreshes never reset FIFO position.
UPDATE coding_queue
SET priority_rank = $2,
    priority_name = $3,
    updated_at = CURRENT_TIMESTAMP
WHERE queue_id = $1;

-- name: MarkQueueAllocated :exec
-- Grant handoff: only a 'waiting' row may accept a grant — this guard is
-- what makes a cancel racing a grant safe (the loser is re-read inside the
-- same advisory-locked transaction and skipped).
UPDATE coding_queue
SET status = 'allocated',
    grant_id = $2,
    blocked_reason = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE queue_id = $1 AND status = 'waiting';

-- name: MarkQueueReleased :exec
UPDATE coding_queue
SET status = 'released',
    grant_id = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE queue_id = $1 AND status = 'allocated';

-- name: CancelQueueEntry :one
-- Atomic cancel of a non-allocated entry. 0 rows means the entry already
-- left the eligible set (allocated/cancelled/released) — the caller must
-- not touch the slot ledger in that case.
UPDATE coding_queue
SET status = 'cancelled',
    blocked_reason = $2,
    updated_at = CURRENT_TIMESTAMP
WHERE queue_id = $1
  AND status IN ('waiting', 'blocked_dependency', 'blocked_unverified')
RETURNING queue_id, run_id, status;

-- name: CountHeldSlots :one
SELECT count(*)::INT AS held
FROM coding_slots
WHERE status = 'held';

-- name: InsertSlotGrant :one
INSERT INTO coding_slots (
    queue_id, run_id, task_id, worker_owner, heartbeat_at
) VALUES (
    $1, $2, $3, $4, CURRENT_TIMESTAMP
)
RETURNING grant_id, queue_id, run_id, task_id, worker_owner, status,
    heartbeat_at, suspected, created_at, released_at, release_reason;

-- name: GetHeldSlotByRun :one
SELECT grant_id, queue_id, run_id, task_id, worker_owner, status,
    heartbeat_at, suspected, created_at, released_at, release_reason
FROM coding_slots
WHERE run_id = $1 AND status = 'held'
LIMIT 1;

-- name: ReleaseSlotGrant :one
-- Guarded release: (run_id, grant_id, held) must all match, so a stale or
-- duplicate stop-confirmation can never release a newer grant and a held
-- slot is released at most once.
UPDATE coding_slots
SET status = 'released',
    released_at = CURRENT_TIMESTAMP,
    release_reason = $3
WHERE run_id = $1 AND grant_id = $2 AND status = 'held'
RETURNING grant_id, queue_id, run_id, task_id, worker_owner, status,
    heartbeat_at, suspected, created_at, released_at, release_reason;

-- name: MarkHeartbeat :one
-- Only the recorded grant may refresh its own heartbeat.
UPDATE coding_slots
SET heartbeat_at = CURRENT_TIMESTAMP,
    suspected = FALSE
WHERE run_id = $1 AND grant_id = $2 AND status = 'held'
RETURNING grant_id, run_id;

-- name: MarkStaleHeartbeatsSuspected :many
-- Flags slots whose heartbeat went quiet. Flagging only — never releases.
UPDATE coding_slots
SET suspected = TRUE
WHERE status = 'held'
  AND suspected = FALSE
  AND heartbeat_at < $1
RETURNING grant_id, run_id;

-- name: InsertSchedulerEvent :one
INSERT INTO scheduler_events (
    event_type, run_id, queue_id, grant_id, actor, reason, metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
RETURNING event_id;

-- name: SchedulerMetrics :many
-- Counts per queue status plus held/suspected slot counts — one scan.
SELECT 'queue_' || status AS bucket, count(*)::BIGINT AS n
FROM coding_queue
GROUP BY status
UNION ALL
SELECT 'slots_held', count(*)::BIGINT FROM coding_slots WHERE status = 'held'
UNION ALL
SELECT 'slots_suspected', count(*)::BIGINT FROM coding_slots WHERE status = 'held' AND suspected;

-- name: GetRunForSchedule :one
-- Minimum run fields the admission path validates (state must exist and be
-- nonterminal; cancelled runs never receive a slot).
SELECT r.run_id, r.task_id, r.state, t.jira_site_id, t.jira_issue_id,
    t.jira_issue_key
FROM task_runs r
JOIN registry_tasks t ON t.id = r.task_id
WHERE r.run_id = $1;
