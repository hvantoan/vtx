-- VTX-8 verification queries. Rows are append-only: the only UPDATE is the
-- guarded 'running' -> terminal finish (or 'superseded' marking on SHA drift).

-- name: InsertVerificationRun :one
-- One row per (run_id, attempt, suite). ON CONFLICT DO NOTHING makes a replay
-- of the same attempt idempotent: no row is returned and the caller re-reads
-- the existing row via GetVerificationRun instead of double-writing.
INSERT INTO verification_runs (
    kind, run_id, attempt, suite, command, subject_commit_sha, status, started_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
ON CONFLICT (run_id, attempt, suite) DO NOTHING
RETURNING kind, verification_id, run_id, attempt, suite, command,
    subject_commit_sha, status, exit_code, duration_ms, log_ref, log_tail,
    log_digest, error_detail, started_at, finished_at, created_at;

-- name: GetVerificationRun :one
SELECT kind, verification_id, run_id, attempt, suite, command,
    subject_commit_sha, status, exit_code, duration_ms, log_ref, log_tail,
    log_digest, error_detail, started_at, finished_at, created_at
FROM verification_runs
WHERE run_id = $1 AND attempt = $2 AND suite = $3
LIMIT 1;

-- name: FinishVerificationRun :one
-- Terminal transition for a 'running' row ONLY. 0 rows means a reconciler or
-- a fence already moved it — the caller treats that as lease/crash loss.
UPDATE verification_runs
SET status = $2,
    exit_code = $3,
    duration_ms = $4,
    log_ref = $5,
    log_tail = $6,
    log_digest = $7,
    error_detail = $8,
    finished_at = $9
WHERE verification_id = $1 AND status = 'running'
RETURNING kind, verification_id, run_id, attempt, suite, command,
    subject_commit_sha, status, exit_code, duration_ms, log_ref, log_tail,
    log_digest, error_detail, started_at, finished_at, created_at;

-- name: ListVerificationRuns :many
SELECT kind, verification_id, run_id, attempt, suite, command,
    subject_commit_sha, status, exit_code, duration_ms, log_ref, log_tail,
    log_digest, error_detail, started_at, finished_at, created_at
FROM verification_runs
WHERE run_id = $1
ORDER BY attempt, started_at;

-- name: GetVerificationSuiteSetAtSHA :many
-- The gate/QA lookup: "does a full suite set exist at THIS sha".
SELECT kind, verification_id, run_id, attempt, suite, command,
    subject_commit_sha, status, exit_code, duration_ms, log_ref, log_tail,
    log_digest, error_detail, started_at, finished_at, created_at
FROM verification_runs
WHERE run_id = $1 AND subject_commit_sha = $2
ORDER BY suite;

-- name: MarkVerificationSuperseded :execrows
-- The ONLY mutation of terminal rows: SHA drift marking (FR-QA-003). Rows
-- bound to a SHA that is not the current one stop counting as evidence.
UPDATE verification_runs
SET status = 'superseded'
WHERE run_id = $1
  AND subject_commit_sha <> $2
  AND status IN ('passed', 'failed', 'skipped');

-- name: ListStaleRunningVerification :many
-- Crash reconcile sweep: rows stuck 'running' whose starter is presumed dead.
SELECT kind, verification_id, run_id, attempt, suite, command,
    subject_commit_sha, status, exit_code, duration_ms, log_ref, log_tail,
    log_digest, error_detail, started_at, finished_at, created_at
FROM verification_runs
WHERE status = 'running' AND started_at < $1
ORDER BY started_at;

-- name: MaxVerificationAttempt :one
-- D8-7: next attempt is computed, not stored on task_runs.
SELECT COALESCE(MAX(attempt), 0)::int AS max_attempt
FROM verification_runs
WHERE run_id = $1;
