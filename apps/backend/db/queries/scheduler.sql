-- VTX-5 coding slot scheduler queries. Mutations run inside the single
-- pgx.Tx owned by PgSchedulerStore (queries.WithTx): pool lock + entry check +
-- run check + grant insert + entry update + audit commit or roll back
-- together — the same boundary discipline as the registry.

-- name: LockCodingSlotPool :one
-- Singleton ledger row. FOR UPDATE serializes every grant transaction across
-- all scheduler replicas; the row also carries max_slots.
SELECT pool_id, max_slots
FROM coding_slot_pool
WHERE pool_id = 1
FOR UPDATE;

-- name: EnqueueCodingEntry :one
-- Idempotent: run_id is UNIQUE, so a repeated enqueue for the same run returns
-- the existing row instead of creating a second queue position.
INSERT INTO coding_queue_entries (run_id, task_id)
VALUES ($1, $2)
ON CONFLICT (run_id) DO NOTHING
RETURNING entry_id, run_id, task_id, fifo_seq, status, block_reason,
    recheck_at, enqueued_at, updated_at;

-- name: GetCodingQueueEntryByRun :one
SELECT entry_id, run_id, task_id, fifo_seq, status, block_reason,
    recheck_at, enqueued_at, updated_at
FROM coding_queue_entries
WHERE run_id = $1
LIMIT 1;

-- name: GetCodingQueueEntryForUpdate :one
SELECT entry_id, run_id, task_id, fifo_seq, status, block_reason,
    recheck_at, enqueued_at, updated_at
FROM coding_queue_entries
WHERE entry_id = $1
FOR UPDATE;

-- name: ListWaitingCodingEntries :many
-- Candidates for the next scheduling pass: still waiting, not parked behind a
-- recheck_at backoff, joined to the run + task for state and Jira identity.
-- Order is the FIFO hint only; the service re-sorts by Jira priority.
SELECT e.entry_id, e.run_id, e.task_id, e.fifo_seq, e.status, e.block_reason,
    e.recheck_at, e.enqueued_at,
    r.state AS run_state,
    t.jira_issue_id, t.jira_issue_key
FROM coding_queue_entries e
JOIN task_runs r ON r.run_id = e.run_id
JOIN registry_tasks t ON t.id = e.task_id
WHERE e.status = 'waiting'
  AND (e.recheck_at IS NULL OR e.recheck_at <= sqlc.arg('now')::timestamptz)
ORDER BY e.fifo_seq ASC
LIMIT sqlc.arg('row_limit')::int;

-- name: SetCodingEntryBlock :exec
-- Records WHY an entry cannot be granted right now and when to re-evaluate.
-- The entry stays 'waiting': a blocked head must never stall later entries.
UPDATE coding_queue_entries
SET block_reason = $2,
    recheck_at = $3,
    updated_at = CURRENT_TIMESTAMP
WHERE entry_id = $1 AND status = 'waiting';

-- name: CloseCodingEntry :one
-- The run is cancelled/terminal or the entry is otherwise dead: it must never
-- receive a slot. Guarded on status so a racing grant cannot be undone.
UPDATE coding_queue_entries
SET status = 'closed',
    block_reason = $2,
    updated_at = CURRENT_TIMESTAMP
WHERE entry_id = $1 AND status = 'waiting'
RETURNING entry_id;

-- name: MarkCodingEntryGranted :exec
UPDATE coding_queue_entries
SET status = 'granted',
    block_reason = NULL,
    recheck_at = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE entry_id = $1 AND status = 'waiting';

-- name: GetRunSchedulingEligibility :one
-- Enqueue guard: the run row (locked FOR UPDATE by the caller's tx) joined
-- to the worktree row that proves preparation is done. has_worktree=true is
-- the "prepared" signal; preparing alone is not enough.
SELECT r.run_id, r.task_id, r.state,
    EXISTS (
        SELECT 1 FROM worktrees w
        WHERE w.run_id = r.run_id AND w.status = 'allocated'
    ) AS has_worktree
FROM task_runs r
WHERE r.run_id = $1
FOR UPDATE OF r;

-- name: GetRunStateForEntry :one
-- Re-check inside the grant tx: a cancel that landed between the candidate
-- list and the grant must win — locked row read, never the stale list.
SELECT r.state
FROM task_runs r
WHERE r.run_id = $1
FOR UPDATE;

-- name: CountActiveSlotGrants :one
SELECT COUNT(*)::int AS active
FROM coding_slot_grants
WHERE status <> 'released';

-- name: ListFreeSlotNumbers :many
-- Slot numbers below max_slots with no non-released grant. Called under the
-- pool row lock so concurrent schedulers cannot pick the same number.
SELECT n::int AS slot_no
FROM generate_series(1, $1::int) AS n
WHERE NOT EXISTS (
    SELECT 1 FROM coding_slot_grants g
    WHERE g.slot_no = n AND g.status <> 'released'
)
ORDER BY n ASC;

-- name: GetActiveGrantForRun :one
SELECT grant_id, slot_no, run_id, task_id, entry_id, owner, session_epoch,
    status, heartbeat_at, release_reason, release_confirmed_at,
    created_at, updated_at
FROM coding_slot_grants
WHERE run_id = $1 AND status <> 'released'
LIMIT 1;

-- name: GetActiveGrantForTask :one
SELECT grant_id, slot_no, run_id, task_id, entry_id, owner, session_epoch,
    status, heartbeat_at, release_reason, release_confirmed_at,
    created_at, updated_at
FROM coding_slot_grants
WHERE task_id = $1 AND status <> 'released'
LIMIT 1;

-- name: CountGrantsForRun :one
SELECT COUNT(*)::bigint AS grants
FROM coding_slot_grants
WHERE run_id = $1;

-- name: InsertSlotGrant :one
INSERT INTO coding_slot_grants (
    slot_no, run_id, task_id, entry_id, owner, session_epoch, status
) VALUES (
    $1, $2, $3, $4, $5, $6, 'held'
)
RETURNING grant_id, slot_no, run_id, task_id, entry_id, owner, session_epoch,
    status, heartbeat_at, release_reason, release_confirmed_at,
    created_at, updated_at;

-- name: GetSlotGrantForUpdate :one
SELECT grant_id, slot_no, run_id, task_id, entry_id, owner, session_epoch,
    status, heartbeat_at, release_reason, release_confirmed_at,
    created_at, updated_at
FROM coding_slot_grants
WHERE grant_id = $1
FOR UPDATE;

-- name: ReleaseSlotGrant :one
-- Guarded release: only a held/suspect grant whose session_epoch matches the
-- caller's fencing token may flip to released. 0 rows means the confirmation
-- was stale or the grant was already released — both are safe no-ops that the
-- caller classifies from the locked row.
UPDATE coding_slot_grants
SET status = 'released',
    release_reason = $2,
    release_confirmed_at = $3,
    updated_at = CURRENT_TIMESTAMP
WHERE grant_id = $1
  AND status <> 'released'
  AND session_epoch = $4
RETURNING grant_id;

-- name: RecordSlotHeartbeat :exec
-- Heartbeat from the executing worker. Epoch match is mandatory: a heartbeat
-- from an old session must not revive bookkeeping for a new grant.
UPDATE coding_slot_grants
SET heartbeat_at = $3,
    updated_at = CURRENT_TIMESTAMP
WHERE grant_id = $1
  AND status <> 'released'
  AND session_epoch = $2;

-- name: ListHeldGrantsStaleHeartbeat :many
-- Suspect sweep: held grants whose heartbeat is missing or older than the
-- cutoff. They keep their slot — suspect marks "needs operator", never
-- "auto-release".
SELECT grant_id, slot_no, run_id, task_id, entry_id, owner, session_epoch,
    status, heartbeat_at, release_reason, release_confirmed_at,
    created_at, updated_at
FROM coding_slot_grants
WHERE status = 'held'
  AND (heartbeat_at IS NULL OR heartbeat_at < sqlc.arg('cutoff')::timestamptz)
ORDER BY created_at;

-- name: MarkGrantSuspect :exec
UPDATE coding_slot_grants
SET status = 'suspect',
    updated_at = CURRENT_TIMESTAMP
WHERE grant_id = $1 AND status = 'held';

-- name: SchedulerMetrics :one
SELECT
    (SELECT COUNT(*)::int FROM coding_queue_entries WHERE status = 'waiting')
        AS queue_waiting,
    (SELECT COUNT(*)::int FROM coding_queue_entries
     WHERE status = 'waiting' AND block_reason IS NOT NULL)
        AS queue_blocked,
    (SELECT COUNT(*)::int FROM coding_slot_grants WHERE status = 'held')
        AS slots_held,
    (SELECT COUNT(*)::int FROM coding_slot_grants WHERE status = 'suspect')
        AS slots_suspect,
    (SELECT max_slots FROM coding_slot_pool WHERE pool_id = 1)
        AS slots_max;

-- name: InsertCodingSlotEvent :exec
INSERT INTO coding_slot_events (
    grant_id, entry_id, run_id, action, actor, reason, metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, sqlc.arg('metadata')::jsonb
);

-- name: ListCodingSlotEventsForRun :many
SELECT event_id, grant_id, entry_id, run_id, action, actor, reason, metadata,
    created_at
FROM coding_slot_events
WHERE run_id = $1
ORDER BY created_at ASC, event_id ASC;
