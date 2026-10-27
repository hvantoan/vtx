-- VTX-9 QA queries. Evidence and verdicts are append-only; the only writes
-- are inserts inside the SubmitVerdict transaction.

-- name: InsertEvidenceArtifact :one
INSERT INTO evidence_artifacts (
    run_id, task_id, attempt, kind, uri, sha256, commit_sha, producer,
    producer_session_id, metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, sqlc.narg('metadata')::jsonb
)
RETURNING artifact_id, run_id, task_id, attempt, kind, uri, sha256,
    commit_sha, producer, producer_session_id, metadata, created_at;

-- name: FindEvidenceArtifactByContent :one
-- Upload idempotency: (uri, sha256) content dedupe — a re-upload of identical
-- content returns the existing row.
SELECT artifact_id, run_id, task_id, attempt, kind, uri, sha256,
    commit_sha, producer, producer_session_id, metadata, created_at
FROM evidence_artifacts
WHERE run_id = $1 AND uri = $2 AND sha256 = $3
  AND commit_sha = $4 AND attempt = $5 AND producer = $6
  AND producer_session_id IS NOT DISTINCT FROM sqlc.narg('session_id')::text
LIMIT 1;

-- name: ListEvidenceByIDs :many
SELECT artifact_id, run_id, task_id, attempt, kind, uri, sha256,
    commit_sha, producer, producer_session_id, metadata, created_at
FROM evidence_artifacts
WHERE artifact_id = ANY(sqlc.arg('artifact_ids')::uuid[]);

-- name: ListEvidenceForSHA :many
SELECT artifact_id, run_id, task_id, attempt, kind, uri, sha256,
    commit_sha, producer, producer_session_id, metadata, created_at
FROM evidence_artifacts
WHERE run_id = $1 AND commit_sha = $2
ORDER BY created_at;

-- name: InsertQAVerdict :one
-- UNIQUE(run_id, attempt) is the one-verdict-per-attempt guard; a second
-- submitter hits 23505 which the caller maps to ErrQAVerdictConflict (409).
INSERT INTO qa_verdicts (
    run_id, task_id, attempt, commit_sha, verdict, findings, qa_session_id,
    qa_agent_identity, coding_session_id, evaluated_at, correlation_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
)
RETURNING verdict_id, run_id, task_id, attempt, commit_sha, verdict,
    findings, qa_session_id, qa_agent_identity, coding_session_id,
    evaluated_at, correlation_id, created_at;

-- name: GetQAVerdict :one
SELECT verdict_id, run_id, task_id, attempt, commit_sha, verdict,
    findings, qa_session_id, qa_agent_identity, coding_session_id,
    evaluated_at, correlation_id, created_at
FROM qa_verdicts
WHERE run_id = $1 AND attempt = $2
LIMIT 1;

-- name: LatestQAVerdictForSHA :one
SELECT verdict_id, run_id, task_id, attempt, commit_sha, verdict,
    findings, qa_session_id, qa_agent_identity, coding_session_id,
    evaluated_at, correlation_id, created_at
FROM qa_verdicts
WHERE run_id = $1 AND commit_sha = $2
ORDER BY evaluated_at DESC
LIMIT 1;

-- name: InsertQAVerdictEvidenceLink :exec
INSERT INTO qa_verdict_evidence (verdict_id, artifact_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListQAVerdictEvidenceIDs :many
SELECT artifact_id FROM qa_verdict_evidence WHERE verdict_id = $1;

-- name: GetRunMetadataValue :one
-- D-9.2: the coding session id rides task_run_events.metadata; the latest
-- event carrying the key wins.
SELECT metadata ->> sqlc.arg('meta_key')::text AS value
FROM task_run_events
WHERE run_id = $1 AND metadata ? sqlc.arg('meta_key')::text
ORDER BY version DESC
LIMIT 1;

-- name: GetQASessionArtifact :one
SELECT * FROM evidence_artifacts
WHERE run_id = $1 AND attempt = $2 AND kind = 'qa_review'
  AND producer = 'system' AND uri = 'qa-session'
ORDER BY created_at DESC LIMIT 1;
