-- +goose Up
-- +goose StatementBegin
-- 00010_quality_gate.sql — deterministic gate decision records.

-- Append-only audit of every gate evaluation. "Effective decision" for a SHA =
-- latest evaluated_at among rows for (run_id, commit_sha). Multiple rows for
-- the same SHA are legal: each re-evaluation is a new audit record. The
-- advance-time guard does not read history — it recomputes — so there is no
-- "current row" to keep consistent.
CREATE TABLE IF NOT EXISTS gate_decisions (
    decision_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    attempt INT NOT NULL,
    -- SHA binding (FR-QA-003): the SHA the decision is about, copied from the
    -- run row locked FOR UPDATE at evaluation time.
    subject_commit_sha TEXT NOT NULL,
    -- The SHA independently re-resolved from the worktree HEAD at evaluation
    -- time (WorktreeReader.HeadSHA). Divergence from subject_commit_sha is
    -- itself a FAIL condition (C2), but both are stored for audit.
    verified_head_sha TEXT,
    policy_name TEXT NOT NULL,        -- e.g. 'vtx-gate-policy'
    policy_version TEXT NOT NULL,     -- e.g. 'v1' — config snapshot, not a FK
    decision TEXT NOT NULL CHECK (decision IN ('pass', 'fail')),
    -- Ordered per-condition results:
    -- [{name, satisfied, blocking, detail?, evidence_artifact_ids[]?}]
    conditions JSONB NOT NULL,
    blocking_reasons TEXT[] NOT NULL DEFAULT '{}',
    evaluator TEXT NOT NULL DEFAULT 'gate-engine',  -- system identity, never human
    correlation_id VARCHAR(128),
    evaluated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_gate_decisions_run_sha
    ON gate_decisions (run_id, subject_commit_sha, evaluated_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_gate_decisions_run_sha;
DROP TABLE IF EXISTS gate_decisions;
-- +goose StatementEnd
