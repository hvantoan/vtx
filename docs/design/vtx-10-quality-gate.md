# VTX-10 — Deterministic Quality Gate Design Spec

**Document status:** Draft for implementation (design lane C)
**SRS:** `docs/VTX_Platform_SRS_v0.1.docx` (VTX-SRS-001 v0.1)
**Scope:** The `quality_gate` stage — a deterministic, evidence-only decision
function that authorizes `quality_gate --advance--> pr_draft` and therefore the
only path to a GitHub Draft PR (FR-PR-001).
**Consumes:** `evidence_artifacts`, `qa_verdicts` (VTX-9), `task_runs` state,
lease/SHA columns (00004).
**Produces:** `gate_decisions` rows, outbox event `quality_gate.decision.v1`,
the `EvidenceGuard` seam that also serves VTX-9's `qa→quality_gate` guard and
VTX-12's `pr_draft→completed` guard.

---

## 1. Overview

FR-QA-002 requires the gate to PASS **only** when mandatory conditions are met —
never on an LLM's opinion alone. The design is therefore a **pure function over
persisted evidence rows**, with three enforcement points:

1. **Decision function.** `GateService.Evaluate(runID)` loads the locked run
   and all evidence bound to its current `commit_sha`, evaluates an ordered,
   versioned list of mandatory conditions, and persists an immutable
   `gate_decisions` audit row + outbox event in the same transaction.
2. **Transition guard.** The `quality_gate --advance--> pr_draft` transition
   cannot commit unless `Evaluate` returns PASS **inside the transition
   transaction**, on the row already locked `FOR UPDATE`. A PASS decision is
   never pre-computed and trusted — it is recomputed at the moment of advance,
   so a SHA change between an earlier PASS and the advance is caught (TOCTOU /
   FR-QA-003).
3. **Operationality (OD-05).** The same guard mechanism decides whether a
   stage hop is "operational": a hop whose downstream system exists but whose
   required evidence is absent returns `ErrEvidenceInvalid` (409, distinct
   from `ErrStageNotOperational`), so a contract-valid transition is never
   reported as run without the evidence it claims (FR-REG-007).

The gate knows nothing about LLMs, prompts, or review text. QA verdict
narrative is evidence metadata; the gate reads only the verdict enum, the SHA
binding, and artifact presence.

---

## 2. SRS Coverage Table

| SRS ID | Requirement (paraphrased) | Design decision |
|---|---|---|
| FR-QA-002 | PASS only when mandatory conditions met; not LLM-opinion-only | `Evaluate` is a deterministic checklist over `gate_policy` v1 conditions (§4.3); every condition resolves to a boolean + artifact/verdict reference. No free text enters the decision. |
| FR-QA-003 | SHA change after QA invalidates prior evidence | Conditions bind to `task_runs.commit_sha` read on the locked row at advance time; all verdicts/evidence are keyed by `commit_sha`; stale-SHA evidence simply fails its condition (§4.3 C3/C4, §5). |
| FR-PR-001 | Draft PR only after gate PASS | The `quality_gate→pr_draft` transition is the single choke point; it runs `Evaluate` in-tx and aborts on non-PASS. There is no second code path that reaches `pr_draft` (§5). |
| FR-REG-005 | Atomic actor/CAS/lease enforcement | Guard executes inside `RegistryStore.TransitionRun`'s tx after `GetTaskRunForUpdate`, before `UpdateTaskRunState`; lease+epoch fencing unchanged (§4.1). |
| FR-REG-007 | Unimplemented downstream stages must not report success | Guard distinguishes three outcomes: `ErrStageNotOperational` (system absent, VTX-3), `ErrEvidenceInvalid` (system exists, evidence missing/stale), pass (evidence verified). |
| FR-ERR-001 | Bounded retry, terminal failure + evidence | A `fail` decision is persisted evidence; the orchestrator maps it to VTX-11 policy (schedule_retry for `evidence_pending`, `fail` for functional fail) (§6). |
| NFR-AUD-001 | Traceability issue→intent→run→SHA→PR | `gate_decisions` stores `commit_sha`, `policy_version`, per-condition artifact IDs, `correlation_id`; the decision row ID lands in `task_run_events.metadata.gate_decision_id` on the advance (§3). |
| NFR-CON-001 | Concurrent writers: exactly one commit | Decisions are append-only rows; the authoritative check is always recomputed on the locked run row, so racing evaluators cannot produce divergent outcomes (§6). |
| NFR-REL-001 | No lost state on crash | Decision + outbox event + (on advance) state update are one tx; a crash before commit leaves the run in `quality_gate` for re-evaluation — evaluation is idempotent because it is a pure read (§6). |
| NFR-OPS-001 | Observable evidence/decisions | `quality_gate.decision.v1` carries the full condition list; `GET /internal/v1/runs/{id}/gate` exposes the latest decision for the dashboard (FR-UI-002). |
| NFR-MAINT-001 | Contract conformance at exact SHA | `GateDecisionRecord` schema is versioned in `contract-drafts/vtx9-11-events.json`; contract test pins it (§7 QG-09). |
| OD-05 | Downstream stages only operational with verified evidence | Resolved by the `EvidenceGuard` design: a stage is marked `operational: true` in the domain table only when its guard is implemented and wired; guards are the definition of "verified evidence" (§8 D-10.1). |

---

## 3. Data Model / DDL Deltas (migrations 00006+)

```sql
-- 00007_quality_gate.sql — deterministic gate decision records.

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
```

No `task_runs` change: `commit_sha`, `version`, lease columns suffice. The run
reaching `pr_draft` is itself proof a PASS existed at advance time; VTX-12
records the resulting PR URL on its own table.

---

## 4. Go Package & API Design

New package: `apps/backend/internal/gate`. Domain additions in
`internal/domain` — one new typed error and the guard seam (the transition
table itself gains `operational: true` rows only where guards exist).

### 4.1 The guard seam (shared VTX-9/10/11/12)

```go
// internal/domain (addition)
// ErrEvidenceInvalid: the hop is contract-valid and its downstream system is
// deployed, but the required evidence is absent, stale, or bound to a
// different SHA. 409. Distinct from ErrStageNotOperational (system absent).
var ErrEvidenceInvalid = errors.New("registry: required evidence missing or stale")

// internal/application (registry_ports.go addition)
// EvidenceGuard runs INSIDE the transition tx, after GetTaskRunForUpdate and
// domain.Evaluate succeed, before UpdateTaskRunState. Registered per
// (from_state, action). A nil guard for an operational hop is a startup
// misconfiguration — the service refuses to boot.
type EvidenceGuard interface {
    Check(ctx context.Context, q Querier /* tx-bound */, locked RunSnapshot, cmd TransitionCommand) error
}
```

`RegistryService.TransitionRun` gains a `map[transitionKey]EvidenceGuard`
injected at construction. Wiring (this lane's responsibility):

| Guarded transition | Guard | Pass condition |
|---|---|---|
| `verifying → qa` | `VerificationEvidenceGuard` | ≥1 `evidence_artifacts(kind='test_results', producer='verifier', commit_sha=locked.commit_sha)` with status metadata `all_green` |
| `qa → quality_gate` | `QAVerdictGuard` | `qa_verdicts` row for `commit_sha`, `verdict ∈ {pass, fail}` |
| `quality_gate → pr_draft` | `GatePassGuard` | `gate.Evaluate` returns PASS (recomputed in-tx) |
| `pr_draft → completed` | `DraftPRGuard` (VTX-12 builds it; the seam lives here) | VTX-12 draft-PR row present for `commit_sha` |

### 4.2 Ports (`internal/gate/ports.go`)

```go
type GateService interface {
    // Evaluate is pure-deterministic: same DB state ⇒ same decision. It
    // persists the decision row + outbox event in the caller's tx when one is
    // supplied (advance path) or its own tx when called standalone (re-check).
    Evaluate(ctx context.Context, q Querier, locked RunSnapshot) (GateDecision, error)
    LatestDecision(ctx context.Context, runID, commitSHA string) (*GateDecision, error)
}

type GateStore interface {
    InsertDecision(ctx context.Context, q Querier, d GateDecision) (GateDecision, error)
    LatestForSHA(ctx context.Context, runID, commitSHA string) (*GateDecision, error)
    // Evidence read side — all scoped to the locked run's commit_sha.
    ListEvidenceForSHA(ctx context.Context, q Querier, runID, sha string) ([]EvidenceArtifact, error)
    LatestVerdictForSHA(ctx context.Context, q Querier, runID, sha string) (*QAVerdictRecord, error)
}

type WorktreeReader interface { /* same seam as vtx-9: HeadSHA(path) */ }

type Clock interface{ Now() time.Time } // reuse
```

### 4.3 Policy `vtx-gate-policy/v1` — mandatory conditions, evaluated in order

Evaluation stops at the first unsatisfied **blocking** condition for speed but
records every condition's result anyway (the audit row lists all conditions;
an un-evaluated tail is recorded as `satisfied=false, detail="not evaluated"` —
never omitted, so a partial checklist can never masquerade as a PASS).

| # | Condition | Satisfied when |
|---|---|---|
| C1 | `run_in_quality_gate` | locked state = `quality_gate`, nonterminal |
| C2 | `sha_binding` | `task_runs.commit_sha` non-null AND `HeadSHA(worktree) == commit_sha` (the run's recorded SHA is the worktree HEAD — nobody moved it) |
| C3 | `verification_green` | `evidence_artifacts` contains `kind='test_results', producer='verifier', commit_sha` with `metadata.status='all_green'` |
| C4 | `qa_verdict_pass` | latest `qa_verdicts` row for `commit_sha` has `verdict='pass'` and `qa_session_id != coding_session_id` (independence re-asserted at gate level, defense in depth) |
| C5 | `qa_executed_tests` | ≥1 `evidence_artifacts(kind='qa_test_run', producer='qa_agent', commit_sha)` linked to that verdict |
| C6 | `lease_fencing` | transition carries valid lease owner+epoch+expiry (already enforced by `domain.Evaluate`; the gate re-records the lease epoch it saw for audit) |
| C7 | `no_superseding_attempt` | `qa_verdicts.attempt == locked.attempt` — verdict belongs to this attempt |

All seven are **blocking**: PASS ⇔ every condition satisfied. There is no
scoring, weighting, or free-text input. Policy name/version are stamped on the
decision row; adding a condition ⇒ bump `policy_version`, old decisions remain
valid history but guards always evaluate the current version.

### 4.4 Internal API (chi)

| Method | Path | Principal | Purpose |
|---|---|---|---|
| POST | `/internal/v1/runs/{run_id}/gate/evaluate` | `orchestrator`, `policy` | Standalone evaluation (persists decision row; does NOT advance) |
| GET  | `/internal/v1/runs/{run_id}/gate/decision?sha=` | `operator`, `orchestrator` | Latest decision for SHA (dashboard evidence, FR-UI-002) |
| GET  | `/internal/v1/runs/{run_id}/gate/history` | `operator` | All decisions for the run (audit) |

The advance path is not a separate endpoint: it is the existing
`TransitionRun` command (`action=advance`, actor=`orchestrator`) whose guard
fires `Evaluate`.

---

## 5. State Machine Interactions (SRS §5)

```
qa ──advance──▶ quality_gate ──advance──▶ pr_draft ──complete──▶ completed
                   ▲                        │
                   │ Evaluate (record)      └── in-tx Evaluate → PASS required
                   │ GatePassGuard aborts with ErrEvidenceInvalid otherwise
```

- On entry to `quality_gate` (after the QA-guarded advance), the orchestrator
  calls `Evaluate` standalone → persists the first `gate_decisions` row +
  `quality_gate.decision.v1`.
- If PASS: orchestrator issues `advance` → `GatePassGuard` recomputes in-tx →
  PASS → transition + audit row carrying `metadata.gate_decision_id` +
  `task_run.state_changed.v1` commit together.
- If FAIL with only `evidence_pending`-class causes (e.g., async `ci_status`
  artifact still absent — see §8 D-10.4): VTX-11 `schedule_retry` →
  `retry_wait` → `retry_due` → back to `quality_gate`, budget-bounded.
- If FAIL functional (`qa_verdict_fail`, test failures): policy issues `fail`
  → terminal `failed` with `error_code='gate_fail'`, decision row is the
  evidence (E2E-04).
- SHA moves after a PASS (shouldn't happen in `quality_gate`, defense in
  depth): `GatePassGuard` recomputation binds to the locked row's current
  `commit_sha`, so the stale PASS is worthless at advance time — E2E-05 is
  enforced structurally, not by cleanup jobs.
- Domain table change: `quality_gate→pr_draft` and `qa→quality_gate` flip to
  `operational: true` once their guards are registered; `verifying→qa` flips
  when VTX-8's verification guard lands. Unimplemented hops keep
  `ErrStageNotOperational` (OD-05 semantics preserved).

---

## 6. Error & Retry Semantics

| Situation | Result |
|---|---|
| Guard evidence missing at advance | `ErrEvidenceInvalid` → 409; caller (orchestrator) decides per VTX-11: `schedule_retry` (evidence_pending) or `fail`. The state does not move. |
| `Evaluate` inside guard sees SHA drift between standalone call and advance | Recomputation catches it — the transition aborts; `run.evidence_invalidated.v1` is emitted by the SHA-writing path. |
| Two orchestrators evaluate concurrently | Both read the same locked row only inside their own txs; decisions are append-only so no lost-update; the advance CAS (`version`) ensures exactly one advance commits (NFR-CON-001). |
| Crash after decision row, before advance | Run still in `quality_gate`; re-evaluation reproduces the identical decision (pure function) → safe replay (NFR-REL-001). |
| Gate input read fails (DB error) | Plain error, not a decision; no row written; caller retries the command — idempotent because nothing was persisted. |
| Policy version bump mid-flight | `policy_version` stamped per decision; guard always runs the **current** version — a PASS under v1 never authorizes an advance under v2 because the guard recomputes, not reads. |

---

## 7. Test Matrix (SRS §9 evidence)

Contributes to §9.1 "VTX-6..12" row and E2E-01/04/05.

| ID | Test | SRS evidence |
|---|---|---|
| QG-01 | Unit: each condition C1–C7 satisfied/violated ⇒ expected decision + blocking_reasons | FR-QA-002 |
| QG-02 | Unit: decision is a pure function — same fixture DB state ⇒ identical conditions output twice | FR-QA-002/NFR-REL-001 |
| QG-03 | Unit: no free-text path — verdict narrative mutated, decision unchanged | FR-QA-002 |
| QG-04 | Integration: advance `quality_gate→pr_draft` without PASS ⇒ `ErrEvidenceInvalid`, version unchanged | FR-PR-001/OD-05 |
| QG-05 | Integration: PASS → advance commits state+audit+outbox atomically; audit metadata has `gate_decision_id` | FR-REG-005/NFR-AUD-001 |
| QG-06 | Integration: verdict exists for SHA-A; run.commit_sha moved to SHA-B ⇒ guard FAIL on C3/C4 (`ErrEvidenceInvalid`) | FR-QA-003/E2E-05 |
| QG-07 | Integration: 20 concurrent `advance` calls on same run ⇒ exactly one commits, 19 `stale_version` | NFR-CON-001 |
| QG-08 | Integration: QA session id == coding session id ⇒ C4 unsatisfied ⇒ decision FAIL | FR-QA-001 defense-in-depth |
| QG-09 | Contract: `quality_gate.decision.v1` + `GateDecisionRecord` validate against contract-draft schema | NFR-MAINT-001 |
| QG-10 | E2E (staging): gate PASS → Draft PR exists; gate FAIL → no PR, `failed` terminal with decision evidence | E2E-01/E2E-04/FR-PR-001 |

---

## 8. Open Decisions

| ID | Decision | Status |
|---|---|---|
| D-10.1 | "Operational" stage = transition-table `operational:true` **and** a registered `EvidenceGuard`; missing guard at boot = startup failure, not silent pass. | **DECIDED** — resolves OD-05 mechanically: a stage can never be "operational" without an evidence check because the service refuses to boot. |
| D-10.2 | Decisions are append-only; the guard recomputes rather than reading the latest row. | **DECIDED** — a stored PASS would be a TOCTOU liability (SHA can drift after it is written); recomputation on the locked row is cheap (7 conditions over indexed rows) and makes E2E-05 structural. |
| D-10.3 | Policy conditions C1–C7 as listed; versioning by `policy_version` string in config, stamped per decision. | **DECIDED (recommended)** — MVP covers SRS MUSTs; extra conditions (coverage threshold, CI status) are additive via version bump. Stakeholder may extend the list without changing the mechanism. |
| D-10.4 | Whether GitHub CI status (`kind='ci_status', producer='github_ci'`) is a mandatory condition in MVP. | **STAKEHOLDER** — CI is async relative to the pipeline; making it blocking couples gate latency to GitHub. Recommended: mandatory if `repo_url` targets GitHub Actions, per policy config flag `require_ci: true` (default false until VTX-12 webhooks exist). Needs owner sign-off. |
| D-10.5 | `inconclusive` QA verdict ⇒ C4 unsatisfied ⇒ gate FAIL (blocking_reason `qa_verdict_not_pass`). | **DECIDED** — a gate that treats "don't know" as pass violates FR-QA-002; the run routes to VTX-11/operator, not to PR. |
| D-10.6 | Who may call standalone `/gate/evaluate`: `orchestrator` + `policy` actors only; operator gets read-only. | **DECIDED** — matches FR-UI-003 (mutations need authorized principals) while keeping decision-writing out of human hands (NFR-SEC-001 least privilege). |
| D-10.7 | Gate runs in-process inside `apps/backend` (not a separate service/Temporal activity). | **DECIDED (recommended)** — the decision must commit inside the transition tx; an external call would break atomicity. Temporal may *invoke* `TransitionRun`, but `Evaluate` stays in the tx. Deployment-as-activity choice remains with the VTX-5/orchestration owner (OD-04 adjacency). |
