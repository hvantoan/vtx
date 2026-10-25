-- name: GetRequirementInput :one
SELECT r.*, t.jira_site_id, t.jira_issue_id, t.jira_issue_key, t.project_key
FROM task_runs r JOIN registry_tasks t ON t.id = r.task_id WHERE r.run_id = $1;

-- name: UpsertRunRequirement :one
INSERT INTO run_requirements (
 run_id, intent_id, jira_site_id, issue_id, issue_key, requirement_version,
 requirement_hash, summary, acceptance_criteria, issue_snapshot, fetched_at
) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11
WHERE EXISTS (SELECT 1 FROM task_runs WHERE task_runs.run_id = $1 AND state = 'preparing')
ON CONFLICT (run_id) DO UPDATE SET
 requirement_version=EXCLUDED.requirement_version, requirement_hash=EXCLUDED.requirement_hash,
 summary=EXCLUDED.summary, acceptance_criteria=EXCLUDED.acceptance_criteria,
 issue_snapshot=EXCLUDED.issue_snapshot, fetched_at=EXCLUDED.fetched_at
RETURNING *;

-- name: GetRunRequirement :one
SELECT * FROM run_requirements WHERE run_id = $1;

-- name: GetRunBaseline :one
SELECT * FROM verification_runs
WHERE run_id=$1 AND kind='baseline' AND subject_commit_sha=$2
ORDER BY attempt DESC LIMIT 1;

-- name: InsertRunBaseline :one
INSERT INTO verification_runs (kind,run_id,attempt,suite,command,subject_commit_sha,status,started_at)
VALUES ('baseline',$1,$2,$3,$4,$5,'running',$6)
ON CONFLICT (run_id,attempt,suite) DO NOTHING RETURNING *;

-- name: FinishRunBaseline :one
UPDATE verification_runs SET status=$2,exit_code=$3,duration_ms=$4,
 log_ref='inline',log_tail=$5,log_digest=$6,finished_at=$7
WHERE verification_id=$1 AND status='running' RETURNING *;

-- name: HasPreparationEvidence :one
SELECT EXISTS (
 SELECT 1 FROM run_requirements rr JOIN worktrees w ON w.run_id=rr.run_id
 JOIN verification_runs v ON v.run_id=w.run_id
 WHERE rr.run_id=$1 AND w.status='allocated' AND v.kind='baseline'
 AND v.subject_commit_sha=w.base_sha AND v.status IN ('passed','failed')
) AS ready;

-- name: ListPreparingRuns :many
SELECT run_id FROM task_runs WHERE state='preparing' ORDER BY created_at,run_id LIMIT $1;

-- name: LockRunBaseline :exec
SELECT pg_advisory_xact_lock(hashtext('vtx-baseline'), hashtext($1::uuid::text));
