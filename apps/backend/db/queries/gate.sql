-- VTX-10 gate decision queries. Rows are append-only audit records; the
-- advance-time guard always recomputes rather than reading the latest row.

-- name: InsertGateDecision :one
INSERT INTO gate_decisions (
    run_id, task_id, attempt, subject_commit_sha, verified_head_sha,
    policy_name, policy_version, decision, conditions, blocking_reasons,
    evaluator, correlation_id, evaluated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, sqlc.arg('conditions')::jsonb, $9, $10, $11, $12
)
RETURNING decision_id, run_id, task_id, attempt, subject_commit_sha,
    verified_head_sha, policy_name, policy_version, decision, conditions,
    blocking_reasons, evaluator, correlation_id, evaluated_at;

-- name: LatestGateDecisionForSHA :one
SELECT decision_id, run_id, task_id, attempt, subject_commit_sha,
    verified_head_sha, policy_name, policy_version, decision, conditions,
    blocking_reasons, evaluator, correlation_id, evaluated_at
FROM gate_decisions
WHERE run_id = $1 AND subject_commit_sha = $2
ORDER BY evaluated_at DESC
LIMIT 1;

-- name: ListGateDecisions :many
SELECT decision_id, run_id, task_id, attempt, subject_commit_sha,
    verified_head_sha, policy_name, policy_version, decision, conditions,
    blocking_reasons, evaluator, correlation_id, evaluated_at
FROM gate_decisions
WHERE run_id = $1
ORDER BY evaluated_at DESC;
