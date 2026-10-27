-- +goose Up
-- +goose StatementBegin
-- VTX-9: evidence + verdicts. All deltas additive (no DROP, no bootstrap
-- rewrite). `commit_sha` is the binding that makes FR-QA-003 enforceable.

-- Immutable evidence artifact. `commit_sha` is the SHA the artifact was
-- produced against — the binding that makes FR-QA-003 enforceable.
CREATE TABLE IF NOT EXISTS evidence_artifacts (
    artifact_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    attempt INT NOT NULL,              -- task_runs.attempt at production time
    kind TEXT NOT NULL CHECK (kind IN (
        'test_results', 'build_log', 'lint_report', 'coverage',
        'qa_review', 'qa_test_run', 'ci_status', 'diff_summary'
    )),
    uri TEXT NOT NULL,                 -- object key / repo-relative path, never a credential-bearing URL
    sha256 TEXT NOT NULL,              -- content hash; tamper check on retrieval
    commit_sha TEXT NOT NULL,          -- exact SHA evaluated (FR-QA-001/003)
    producer TEXT NOT NULL CHECK (producer IN (
        'coding_agent', 'verifier', 'qa_agent', 'github_ci', 'system'
    )),
    producer_session_id TEXT,          -- agent session id when producer is an agent
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_evidence_artifacts_run_sha
    ON evidence_artifacts (run_id, commit_sha);

-- One verdict per (run_id, attempt): an attempt produces exactly one QA
-- outcome. A new attempt (retry) gets a fresh row; history is append-only.
CREATE TABLE IF NOT EXISTS qa_verdicts (
    verdict_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES registry_tasks(id) ON DELETE CASCADE,
    attempt INT NOT NULL,
    commit_sha TEXT NOT NULL,          -- the exact SHA the QA agent evaluated
    verdict TEXT NOT NULL CHECK (verdict IN ('pass', 'fail', 'inconclusive')),
    findings JSONB NOT NULL DEFAULT '[]',
    qa_session_id TEXT NOT NULL,       -- VTX-7 session id of the QA agent
    qa_agent_identity TEXT NOT NULL,   -- e.g. "qa-agent:pi@0.x" — never the coding principal
    coding_session_id TEXT,            -- snapshot for the independence assertion
    evaluated_at TIMESTAMPTZ NOT NULL,
    correlation_id VARCHAR(128),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT qa_verdicts_attempt_unique UNIQUE (run_id, attempt)
);

-- QA evidence must include at least one row with producer='qa_agent' and
-- kind='qa_test_run' (the QA-executed test run). Enforced in the service layer,
-- not as a constraint, so supplementary evidence can arrive independently.
CREATE INDEX IF NOT EXISTS idx_qa_verdicts_run_sha
    ON qa_verdicts (run_id, commit_sha);

CREATE TABLE IF NOT EXISTS qa_verdict_evidence (
    verdict_id UUID NOT NULL REFERENCES qa_verdicts(verdict_id) ON DELETE CASCADE,
    artifact_id UUID NOT NULL REFERENCES evidence_artifacts(artifact_id) ON DELETE RESTRICT,
    PRIMARY KEY (verdict_id, artifact_id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS qa_verdict_evidence;
DROP TABLE IF EXISTS qa_verdicts;
DROP TABLE IF EXISTS evidence_artifacts;
-- +goose StatementEnd
