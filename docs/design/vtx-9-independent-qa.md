# VTX-9 — Independent QA Design Spec

**Document status:** Draft for implementation (design lane C)
**SRS:** `docs/VTX_Platform_SRS_v0.1.docx` (VTX-SRS-001 v0.1)
**Scope:** Independent QA stage — a QA agent with an identity distinct from the
coding agent evaluates the **exact commit SHA** plus test evidence and records a
verdict that the VTX-10 Quality Gate consumes.
**Consumes:** run state `qa` (SRS §5), `task_runs.commit_sha`, worktree/lease
(VTX-4), agent runtime (VTX-7), verification evidence (VTX-8).
**Produces:** `qa_verdicts`, `evidence_artifacts` rows, outbox event
`qa.verdict_recorded.v1`.

---

## 1. Overview

FR-QA-001 requires that QA is performed by an agent that is **not the same
identity** as the coding agent, and that the evaluation targets the **exact
commit SHA** the run produced, together with test evidence. The design therefore
splits QA into three concerns:

1. **Evidence capture** — every artifact that substantiates a verdict (test
   output, build log, coverage, QA review notes) is persisted as an immutable
   `evidence_artifacts` row bound to `commit_sha` and `run_id`/`attempt`.
2. **Independent evaluation** — a QA agent session is launched through the
   VTX-7 agent runtime with a dedicated `qa` profile: separate session ID,
   separate principal identity (`qa-agent`), read-only tooling (no commit, no
   push, no writes outside a scratch area). The QA agent re-runs the test
   baseline in the run's worktree, checked out at the exact `commit_sha`
   recorded on `task_runs` at launch time.
3. **Verdict recording** — the verdict is a database row, not a chat message.
   `QAService.SubmitVerdict` writes `qa_verdicts` + evidence links + audit +
   outbox event in one transaction, using the same
   claim → mutate → audit → outbox pattern as `RegistryService` (VTX-3).

The QA verdict does **not** drive the run forward by itself. The orchestrator
advances `qa --advance--> quality_gate` only after a verdict row exists for the
run's current `commit_sha`; the transition guard (shared with VTX-10, see
`vtx-10-quality-gate.md`) enforces this atomically inside the transition
transaction.

Independence is enforced at three levels:

| Level | Mechanism |
|---|---|
| Identity | QA runs under agent profile `qa` with session principal `qa-agent`; the verdict row stores `qa_session_id` and `qa_agent_identity`, and the service rejects a verdict whose QA session ID equals the recorded coding session ID. |
| Data | QA receives the worktree path + `commit_sha` + test baseline manifest — never the coding agent's transcript, self-assessment, or rationale. |
| Temporal | QA re-executes tests itself; coding-agent test output is admissible as *supplementary* evidence (producer = `coding_agent`) but never satisfies the mandatory "QA-executed test run" condition. |

---

## 2. SRS Coverage Table

| SRS ID | Requirement (paraphrased) | Design decision |
|---|---|---|
| FR-QA-001 | Independent QA, distinct identity, evaluates exact commit SHA + test evidence | QA runs via VTX-7 runtime under profile `qa` / principal `qa-agent` (§4 `QAProfile`); session identity asserted distinct from coding session in `SubmitVerdict`; verdict is bound to `commit_sha` column on `qa_verdicts` (DDL §3). |
| FR-QA-003 | SHA change after QA invalidates prior evidence | All evidence + verdicts keyed by `commit_sha`; transition guard for `qa→quality_gate` and `quality_gate→pr_draft` requires `verdict.commit_sha == task_runs.commit_sha` evaluated on the row locked FOR UPDATE; `run.evidence_invalidated.v1` event emitted when SHA moves (§5, §6). |
| FR-AGT-002 | Agent host: handshake, cancellation, streaming, cleanup | QA session reuses the VTX-7 `AgentRunner` port with `QAProfile`; cancellation is invoked on run `cancel`/`fail` transitions (§4, §6). |
| FR-REG-005 | Transitions enforce actor/CAS/lease atomically | The `qa→quality_gate` guard executes inside the same `pgx.Tx` that already locks the run row; no guard decision is taken outside the lock (§5). |
| FR-ERR-001 | Bounded retry, terminal failure with evidence | QA session crashes classify as `agent_crashed`/`lease_lost` (retryable per VTX-11 policy); a `fail` verdict is a *functional* outcome, not an infra failure — see open decision D-9.4 (§8). |
| NFR-AUD-001 | Traceability issue→intent→run→SHA→PR; no secrets | Every verdict row carries `correlation_id`, `run_id`, `commit_sha`, `qa_session_id`; audit `task_run_events` gets `metadata.qa_verdict_id` on the guarded advance. Evidence `uri` fields store object keys, never credentials. |
| NFR-CON-001 | 20+ concurrent writers, single commit wins | Verdict submission runs in one tx with `SELECT … FOR UPDATE` on the run row; second submitter for the same `(run_id, attempt)` hits the unique index → 409 (`ErrQAVerdictConflict`). |
| NFR-REL-001 | No lost state on crash | Verdict + evidence + outbox event commit atomically; a QA crash before commit leaves the run in `qa` for the VTX-11 recovery sweeper. |
| NFR-MAINT-001 | Contract conformance at exact SHA | `qa.verdict_recorded.v1` is schema-versioned in `contract-drafts/vtx9-11-events.json`; a contract test pins payload shape to the schema file. |
| NFR-SEC-001 | Least privilege, fail-closed internal API | `SubmitVerdict` is reachable only on the internal listener with the `qa-agent` principal; forged `qa-agent` identity is stripped at the gateway like other internal-auth headers (FR-JIRA-007 pattern). |

No SRS requirement in scope is dropped; items shared with VTX-10/VTX-11 are
cross-referenced rather than duplicated.

---

## 3. Data Model / DDL Deltas (migration 00009)

All deltas are additive (OD-02 discipline: no DROP, no bootstrap-data rewrite).

```sql
-- 00009_qa.sql — evidence + verdicts for VTX-9.

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
    -- findings[]: {severity: blocker|major|minor|info, category, summary,
    --              path?, line?, evidence_artifact_id?}
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
```

`task_runs` needs no new columns for VTX-9: `commit_sha`, `attempt`, and
`correlation_id` already exist (00004).

---

## 4. Go Package & API Design

New package: `apps/backend/internal/qa` (application service) with domain
additions in `internal/domain` (types only — the state machine table is not
modified by VTX-9 except via the shared evidence guard, specified in the VTX-10
doc).

### 4.1 Domain types (`internal/domain/qa.go`)

```go
// QAVerdict is the persisted outcome. Wire values are contract-frozen.
type QAVerdict string
const (
    QAVerdictPass         QAVerdict = "pass"
    QAVerdictFail         QAVerdict = "fail"
    QAVerdictInconclusive QAVerdict = "inconclusive"
)

type EvidenceKind string // test_results | build_log | lint_report | coverage |
                         // qa_review | qa_test_run | ci_status | diff_summary
type EvidenceProducer string // coding_agent | verifier | qa_agent | github_ci | system

// Typed errors (HTTP status via HTTPStatusForError extension):
var (
    ErrQAVerdictConflict  = errors.New("qa: verdict already recorded for attempt")   // 409
    ErrQASessionNotUnique = errors.New("qa: qa session identical to coding session") // 409
    ErrQAEvidenceMissing  = errors.New("qa: mandatory qa_test_run evidence absent")  // 409
    ErrQASHAMismatch      = errors.New("qa: verdict commit_sha does not match run")  // 409
)
```

### 4.2 Ports (`internal/qa/ports.go`)

```go
// QAService is the only write path for QA outcomes (mirrors RegistryService).
type QAService interface {
    LaunchSession(ctx context.Context, cmd LaunchQACommand) (QASession, error)
    SubmitVerdict(ctx context.Context, cmd SubmitVerdictCommand) (QAVerdictRecord, error)
    VerdictForSHA(ctx context.Context, runID string, commitSHA string) (*QAVerdictRecord, error)
    ListEvidence(ctx context.Context, runID string, commitSHA string) ([]EvidenceArtifact, error)
}

type QAStore interface {
    // SubmitVerdictTx runs the whole verdict write — run lock, SHA check,
    // independence check, verdict + evidence links + audit metadata + outbox
    // row — in one pgx.Tx.
    SubmitVerdictTx(ctx context.Context, cmd SubmitVerdictCommand, now time.Time) (QAVerdictRecord, error)
    GetVerdict(ctx context.Context, runID string, attempt int) (*QAVerdictRecord, error)
    LatestVerdictForSHA(ctx context.Context, runID string, commitSHA string) (*QAVerdictRecord, error)
    ListEvidence(ctx context.Context, runID string, commitSHA string) ([]EvidenceArtifact, error)
    InsertEvidence(ctx context.Context, ev EvidenceArtifact) (EvidenceArtifact, error)
}

// AgentRunner is the VTX-7 seam (JSON-RPC 2.0 stdio). VTX-9 supplies the
// profile, not a new transport.
type AgentRunner interface {
    StartSession(ctx context.Context, profile AgentProfile, work AgentWorkSpec) (AgentSession, error)
    CancelSession(ctx context.Context, sessionID string, reason string) error
}

// WorktreeReader gives QA read access to the run worktree pinned at SHA.
type WorktreeReader interface {
    PathForRun(ctx context.Context, runID string) (string, error)            // worktrees.path
    HeadSHA(ctx context.Context, worktreePath string) (string, error)      // `git rev-parse HEAD`
    CheckoutSHA(ctx context.Context, worktreePath, sha string) error       // detached checkout
}
```

### 4.3 Commands

```go
type LaunchQACommand struct {
    RunID           string
    ExpectedVersion int64   // CAS: the run must still be in `qa` at this version
}

type SubmitVerdictCommand struct {
    RunID           string
    CommitSHA       string  // must equal task_runs.commit_sha on the locked row
    Verdict         domain.QAVerdict
    Findings        []domain.QAFinding
    EvidenceIDs     []string // must already exist as evidence_artifacts rows
    // Producers (VTX-8 verifier, the QA agent itself) write artifact rows via
    // QAStore.RecordEvidence INSIDE their own write tx — the ingestion path is
    // service-level in MVP (all producers in-process); no HTTP endpoint.
    QASessionID     string  // from the AgentRunner session
    QAIdentity      string  // authenticated principal, e.g. "qa-agent"
    CodingSessionID string  // snapshot recorded at launch
    CorrelationID   string
}
```

### 4.4 Submission rules (all inside `SubmitVerdictTx`)

1. `GetTaskRunForUpdate` → run must exist, state `qa`, nonterminal.
2. `cmd.CommitSHA == locked.commit_sha` else `ErrQASHAMismatch` (FR-QA-003).
3. `cmd.QASessionID != locked coding session id` else `ErrQASessionNotUnique`
   (coding session ID is recorded on `qa_verdicts.coding_session_id` at launch
   and carried in run metadata; see §8 D-9.2 for the storage seam).
4. `EvidenceIDs` must include ≥1 artifact with `producer='qa_agent'` and
   `kind='qa_test_run'` whose `commit_sha` matches, else `ErrQAEvidenceMissing`.
5. Insert verdict (`UNIQUE(run_id, attempt)` — second submitter gets
   `ErrQAVerdictConflict`, mapped to 409).
6. Insert `qa_verdict_evidence` join rows; append outbox event
   `qa.verdict_recorded.v1` in the same tx.

### 4.5 Internal API (transport, chi)

| Method | Path | Principal | Purpose |
|---|---|---|---|
| POST | `/internal/v1/runs/{run_id}/qa/session` | `orchestrator` | Launch QA session (idempotent on run+attempt) |
| POST | `/internal/v1/runs/{run_id}/qa/verdict` | `qa-agent` | Submit verdict (+evidence IDs) |
| GET  | `/internal/v1/runs/{run_id}/qa/verdict?sha=` | `operator`, `orchestrator` | Read verdict for exact SHA |
| GET  | `/internal/v1/runs/{run_id}/evidence?sha=` | `operator`, `orchestrator` | List evidence for dashboard (FR-UI-002) |

`POST …/qa/verdict` authenticates the `qa-agent` internal principal exactly the
way webhook internal-auth is verified (fail-closed; NFR-SEC-001). The QA agent
never calls `TransitionRun` — it is an evidence producer, not a transition
actor, so the actor enum is unchanged (see §8 D-9.1).

---

## 5. State Machine Interactions (SRS §5)

```
coding ──advance──▶ verifying ──advance──▶ qa ──advance──▶ quality_gate
                     │                      │  (guard: verdict exists
                     │                      │   for current commit_sha)
                     ▼                      ▼
            evidence bound to SHA      verdict bound to SHA
```

- Entry: `verifying --advance--> qa` becomes operational when VTX-8 verification
  evidence exists for `task_runs.commit_sha` (guard owned by VTX-10 spec, shared
  implementation).
- While in `qa`: `QAService.LaunchSession` requires the locked run to be in
  `qa` at `ExpectedVersion` (CAS) with a valid lease — the launch is a lease-
  requiring side effect performed by the orchestrator actor.
- Exit: `qa --advance--> quality_gate` (actor `orchestrator`, lease required)
  is guarded: a `qa_verdicts` row with `commit_sha == locked.commit_sha` and
  `verdict IN ('pass','fail')` must exist. `inconclusive` blocks the advance and
  is surfaced to the operator (treated like `blocked`, not retried blindly).
- SHA change while in `qa` (coding agent amends post-verification — abnormal
  but possible): the pending verdict submission fails the SHA check
  (`ErrQASHAMismatch`); any previously recorded verdict stays in history but no
  guard accepts it. The VTX-8 commit-recording path emits
  `run.evidence_invalidated.v1` (contract draft).
- `block`/`cancel`/`fail`/`schedule_retry` behave per the existing table;
  `retry_wait --retry_due--> qa` re-enters QA and requires a *new* verdict for
  the same or new SHA (verdicts are keyed `(run_id, attempt)`).

---

## 6. Error & Retry Semantics

| Failure | Class | Handling |
|---|---|---|
| QA agent process crash / timeout | `agent_crashed` (retryable) | `CancelSession`, then VTX-11 policy `schedule_retry`; run parks in `retry_wait`, resumes to `qa`. |
| Lease lost mid-session | `lease_lost` (retryable-bounded) | Verdict submission fails fencing; recovery sweeper applies VTX-11 policy. |
| Verdict SHA mismatch | `ErrQASHAMismatch` | 409; no retry of the write — orchestrator relaunches QA only if run still expects a verdict, else treats as `agent_error`. |
| Duplicate verdict (attempt already has one) | `ErrQAVerdictConflict` | 409; idempotent-read path returns the existing verdict to the caller. |
| Missing QA-executed evidence | `ErrQAEvidenceMissing` | 409; orchestrator relaunches or fails per attempt budget. |
| `inconclusive` verdict | non-error | Run stays in `qa`; emits `task_run.state_changed.v1`-adjacent audit via verdict event; operator decides block/reschedule (STAKEHOLDER default, §8 D-9.3). |

Evidence upload is idempotent by content: `(uri, sha256)` dedupe in service;
artifact rows are insert-only — re-upload of identical content returns the
existing row (matches the `worktrees` idempotency pattern in 00005).

---

## 7. Test Matrix (SRS §9 evidence)

Contributes to §9.1 "VTX-6..12" row and E2E-01/04/05.

| ID | Test | SRS evidence |
|---|---|---|
| QA-01 | Unit: verdict submission happy path writes verdict+join+outbox atomically | FR-QA-001 |
| QA-02 | Unit: SHA mismatch on submit → `ErrQASHAMismatch`, no rows written | FR-QA-003 / E2E-05 |
| QA-03 | Unit: `qa_session_id == coding_session_id` → `ErrQASessionNotUnique` | FR-QA-001 independence |
| QA-04 | Unit: verdict without `qa_test_run` evidence → `ErrQAEvidenceMissing` | FR-QA-001 "test evidence" |
| QA-05 | Integration: two concurrent submits same attempt → exactly one row, second gets 409 | NFR-CON-001 |
| QA-06 | Integration: `qa→quality_gate` advance rejected before verdict; accepted after | FR-REG-005/OD-05 |
| QA-07 | Integration: new commit_sha recorded after verdict → guard rejects advance; `run.evidence_invalidated.v1` in outbox | E2E-05 |
| QA-08 | Integration: crash between verdict tx and ACK → replay is no-op (inbox receipt) | NFR-REL-001 / FR-EVT-003 |
| QA-09 | Contract test: `qa.verdict_recorded.v1` payload validates against `contract-drafts/vtx9-11-events.json` | NFR-MAINT-001 |
| QA-10 | E2E (staging): real QA session re-runs tests in worktree @ SHA, verdict recorded, dashboard shows evidence | E2E-01 |

---

## 8. Open Decisions

| ID | Decision | Status |
|---|---|---|
| D-9.1 | QA agent is an evidence producer, **not** a new transition actor; the `orchestrator` actor still drives `qa→quality_gate`, gated on the verdict row. | **DECIDED** — keeps FR-REG-005's authorization surface closed; adding an actor would widen every transition-table check for no audit gain (session identity is already captured on the verdict). |
| D-9.2 | Coding-session identity is carried in `task_run_events.metadata.coding_session_id` (written by the VTX-7 coding-session record) and snapshotted onto `qa_verdicts.coding_session_id`; no new `task_runs` column. | **DECIDED** — metadata is already the audit-extension seam; a column would be redundant until a query needs it (YAGNI). |
| D-9.3 | `inconclusive` verdict leaves the run parked in `qa` for operator decision rather than auto-retry. | **DECIDED (recommended)** — inconclusive means "evidence insufficient," which is a human-judgment boundary, not an infra flake; bounded auto-retry of inconclusive risks verdict-shopping. Flagged for stakeholder sign-off because it adds an operator step to the pipeline. |
| D-9.4 | A `fail` QA verdict does **not** auto-retry coding within the same run; the gate records `fail` and VTX-11 marks the run `failed` (terminal) with evidence. | **STAKEHOLDER** — E2E-04 says "retries bounded," but SRS §5 has no backward transition edge (qa→coding). Two readings exist: (a) retry = transient stage re-run only [recommended], or (b) extend `schedule_retry` with a backward `retry_target` (specified in `vtx-11-retry-orchestration.md`, needs owner approval since it amends VTX-3 domain semantics). |
| D-9.5 | Evidence `uri` backend for MVP: filesystem path under the run worktree's `.vtx/evidence/` dir, recorded as repo-relative key. | **DECIDED (recommended)** — object storage is a later swap behind `EvidenceStore`; `sha256` already makes retrieval verifiable. |
| D-9.6 | Whether QA must also review the diff (code review) beyond test re-execution. | **STAKEHOLDER** — SRS says "code/tests/evidence" evaluated; recommended MVP scope = test re-execution + structured findings free-form, because LLM review text cannot be a gate condition anyway (FR-QA-002). |
