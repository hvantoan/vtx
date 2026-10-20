-- VTX-3 registry queries. Every mutation runs inside the single pgx.Tx opened
-- by the application service (queries.WithTx), so claim → transition → audit →
-- outbound event either all commit or none do (TR-17/TR-25).

-- name: UpsertRegistryTask :one
-- Identity upsert on (jira_site_id, jira_issue_id). jira_issue_key/project_key
-- are mutable metadata; the identity and task version are not touched here.
INSERT INTO registry_tasks (
    jira_site_id, jira_issue_id, jira_issue_key, project_key
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (jira_site_id, jira_issue_id) DO UPDATE
SET jira_issue_key = EXCLUDED.jira_issue_key,
    project_key = EXCLUDED.project_key,
    updated_at = CURRENT_TIMESTAMP
RETURNING id, jira_site_id, jira_issue_id, jira_issue_key, project_key,
    version, created_at, updated_at;

-- name: GetRegistryTaskByIdentity :one
SELECT id, jira_site_id, jira_issue_id, jira_issue_key, project_key,
    version, created_at, updated_at
FROM registry_tasks
WHERE jira_site_id = $1 AND jira_issue_id = $2
LIMIT 1;

-- name: InsertTaskRun :one
-- run_id comes from gen_random_uuid() — never from the Go process.
INSERT INTO task_runs (
    task_id, intent_id, correlation_id, origin_event_id, changelog_id, state
) VALUES (
    $1, $2, $3, $4, $5, 'queued'
)
RETURNING run_id, task_id, intent_id, correlation_id, origin_event_id,
    changelog_id, state, version, attempt, resume_state, retry_due_at,
    commit_sha, branch, error_code, error_message, lease_owner, lease_epoch,
    lease_expires_at, created_at, updated_at, started_at, finished_at;

-- name: HasActiveRunForTask :one
-- I-03 read side: the partial unique index is the enforcement, this is the
-- branch selector for admit-vs-defer.
SELECT EXISTS (
    SELECT 1 FROM task_runs
    WHERE task_id = $1
      AND state NOT IN ('completed', 'failed', 'cancelled')
) AS has_active;

-- name: GetTaskRun :one
SELECT run_id, task_id, intent_id, correlation_id, origin_event_id,
    changelog_id, state, version, attempt, resume_state, retry_due_at,
    commit_sha, branch, error_code, error_message, lease_owner, lease_epoch,
    lease_expires_at, created_at, updated_at, started_at, finished_at
FROM task_runs
WHERE run_id = $1
LIMIT 1;

-- name: GetTaskRunForUpdate :one
-- The row lock that makes the §0.D error ordering deterministic: the first
-- committer wins, every other writer blocks here, then reads the NEW version
-- and is classified as stale_version.
SELECT run_id, task_id, intent_id, correlation_id, origin_event_id,
    changelog_id, state, version, attempt, resume_state, retry_due_at,
    commit_sha, branch, error_code, error_message, lease_owner, lease_epoch,
    lease_expires_at, created_at, updated_at, started_at, finished_at
FROM task_runs
WHERE run_id = $1
FOR UPDATE;

-- name: UpdateTaskRunState :one
-- CAS on version. Called only after GetTaskRunForUpdate; `WHERE version = $10`
-- is the second line of defence (0 rows ⇒ caller treats it as stale_version).
UPDATE task_runs
SET state = $2,
    version = version + 1,
    resume_state = $3,
    retry_due_at = $4,
    attempt = $5,
    error_code = $6,
    error_message = $7,
    started_at = $8,
    finished_at = $9,
    updated_at = CURRENT_TIMESTAMP
WHERE run_id = $1 AND version = $10
RETURNING run_id, task_id, intent_id, correlation_id, origin_event_id,
    changelog_id, state, version, attempt, resume_state, retry_due_at,
    commit_sha, branch, error_code, error_message, lease_owner, lease_epoch,
    lease_expires_at, created_at, updated_at, started_at, finished_at;

-- name: InsertTaskRunEvent :one
-- I-05: the creation row is version 0 with previous_state = '' (a run is born,
-- it does not come from a state).
INSERT INTO task_run_events (
    run_id, previous_state, current_state, action, actor, actor_type,
    correlation_id, version, metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, sqlc.arg('metadata')::jsonb
) RETURNING event_id;

-- name: ListTaskRunEvents :many
-- Cursor on version, and the creation audit (version 0) must be reachable:
-- a NULL cursor means "from the beginning", not "after 0".
SELECT event_id, run_id, previous_state, current_state, action, actor,
    actor_type, correlation_id, version, metadata, created_at
FROM task_run_events
WHERE run_id = $1
  AND (sqlc.narg('after_version')::bigint IS NULL OR version > sqlc.narg('after_version')::bigint)
ORDER BY version ASC
LIMIT sqlc.arg('row_limit')::int;

-- name: InsertDeferredIntent :one
-- ON CONFLICT DO NOTHING ⇒ replaying the same intent yields no row (the
-- inbox receipt already deduped it, this is belt and braces).
INSERT INTO deferred_intents (
    intent_id, task_id, disposition, reason
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (intent_id) DO NOTHING
RETURNING intent_id, task_id, received_at, disposition, reason;

-- name: ListDeferredIntents :many
-- FIFO order is the contract for the admission path (VTX-5).
SELECT intent_id, task_id, received_at, disposition, reason
FROM deferred_intents
WHERE disposition = 'pending_deferred'
ORDER BY received_at, intent_id
LIMIT sqlc.arg('row_limit')::int;

-- name: InsertInboxConsumption :one
-- Exactly-once persisted effect per (consumer, source event). Duplicate ⇒
-- pgx.ErrNoRows, which the caller reads as "already consumed".
INSERT INTO inbox_consumptions (
    consumer_name, source_event_id, run_id
) VALUES (
    $1, $2, $3
)
ON CONFLICT (consumer_name, source_event_id) DO NOTHING
RETURNING consumer_name, source_event_id, run_id, consumed_at;

-- name: LinkInboxConsumptionToRun :exec
-- The receipt is written first (it is the dedup gate, before we know whether a
-- run is admitted); this links it to the run once the run exists, so the audit
-- trail can answer "which run did this event create" without a payload join.
UPDATE inbox_consumptions
SET run_id = $3
WHERE consumer_name = $1 AND source_event_id = $2;
