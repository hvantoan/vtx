-- VTX-ORCH orchestration launcher claims. worker_heartbeats already models
-- "one row per long-lived driver", so an orchestrator process claims one run by
-- inserting a row keyed (run_id, 'agent_host'). The claim token in metadata
-- makes beat/release conditional: a relauncher that took over after a stale
-- window can never have its claim deleted by the crashed predecessor.

-- name: ClaimOrchestratorRun :one
INSERT INTO worker_heartbeats AS h (worker_id, worker_kind, run_id, queue, active_runs, metadata)
VALUES (
    sqlc.arg(worker_id),
    'agent_host',
    sqlc.arg(run_id),
    'coding',
    1,
    jsonb_build_object('claim', sqlc.arg(claim_token)::text)
)
ON CONFLICT (worker_id, worker_kind) DO UPDATE
SET last_beat_at = CURRENT_TIMESTAMP,
    run_id = EXCLUDED.run_id,
    active_runs = 1,
    metadata = EXCLUDED.metadata,
    started_at = CURRENT_TIMESTAMP
WHERE h.last_beat_at < sqlc.arg(stale_before)
RETURNING worker_id;

-- name: BeatOrchestratorRun :execrows
UPDATE worker_heartbeats
SET last_beat_at = CURRENT_TIMESTAMP
WHERE worker_id = sqlc.arg(worker_id)
  AND worker_kind = 'agent_host'
  AND metadata->>'claim' = sqlc.arg(claim_token)::text;

-- name: ReleaseOrchestratorRun :execrows
DELETE FROM worker_heartbeats
WHERE worker_id = sqlc.arg(worker_id)
  AND worker_kind = 'agent_host'
  AND metadata->>'claim' = sqlc.arg(claim_token)::text;
