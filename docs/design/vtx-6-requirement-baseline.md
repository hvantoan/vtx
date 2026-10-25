# VTX-6 — Requirement Extraction & Test Baseline — Design Specification

**Document status:** Draft for implementation (design lane A, coordinator-written)
**SRS:** `docs/VTX_Platform_SRS_v0.1.docx` (VTX-SRS-001 v0.1)
**Scope:** turning a Jira issue into the *dispatch packet* a coding agent runs
against — the normalized requirement plus the test baseline recorded on the
run's `base_sha` **before** the agent touches code.
**Consumes:** `task_runs` (VTX-3), worktree allocation (VTX-4), `FetchIssue`
(`internal/integrations/jira`), `verification_runs` kind=`baseline` (VTX-8
table, `00007`), `evidence_artifacts` (VTX-9 table, `00006`).
**Produces:** `run_requirements` rows, baseline `verification_runs` rows,
outbox events `run.requirement_ready.v1` / `run.baseline_recorded.v1`, the
`AgentDispatch` payload consumed by VTX-7.

---

## 1. Overview

FR-AGT-001 says the coding agent receives *scope, repo/worktree, and a test
baseline* — and emits artifacts/commit SHA/logs. VTX-6 is the producer of the
first two inputs:

1. **Requirement extraction.** On `preparing`, fetch the Jira issue through the
   existing `IssueFetcher` port (never trust webhook payload — FR-JIRA-003
   precedent): summary, description, acceptance criteria, linked/sub issues,
   labels, priority. Normalize into a versioned `RequirementPacket` and persist
   it (`run_requirements`) so the agent prompt, QA review, and gate audit all
   read the *same* frozen requirement (NFR-AUD-001).
2. **Test baseline.** After worktree allocation, run the project baseline
   suite once on `base_sha` and record it as a `verification_runs` row
   `kind='baseline'`. QA (VTX-9) later compares agent-run suite results against
   this baseline instead of absolute green — an environment-broken baseline is
   evidence, not a silent assumption.

The `preparing --advance--> coding` hop flips `operational` only when both
artifacts exist for the run: requirement row **and** baseline row. That is the
OD-05-style guard this epic contributes to `domain.transitionTable` (same
`EvidenceGuard` seam VTX-10 defines).

---

## 2. SRS Coverage Table

| SRS ID | Requirement | Design decision |
|---|---|---|
| FR-AGT-001 | Agent gets scope + repo/worktree + test baseline; emits artifacts/SHA/logs | `AgentDispatch` struct (§4.3) is the single dispatch contract: `run_id`, `worktree_path`, `branch`, `base_sha`, `requirement` (frozen packet), `baseline` (suite id + status + artifact refs), `attempt`. Agent outputs stay in VTX-7/8. |
| FR-JIRA-003 | Re-fetch identity/state before trusting | Same `IssueFetcher` port, same bounded retry + redaction as intake; extraction records `fetched_at` so QA can tell stale packets. |
| FR-REG-007 | Downstream stages must not fake-run | `preparing→coding` guard requires `run_requirements` + baseline `verification_runs` rows for `run_id`/`base_sha`; missing ⇒ `ErrEvidenceInvalid` (409), not a silent pass (§5). |
| FR-QA-002/003 | Gate needs real evidence bound to SHA | Baseline is keyed to `subject_commit_sha=base_sha`; QA's post-agent run is keyed to agent SHA; the comparison is explicit in the gate condition (VTX-10 C4). |
| NFR-AUD-001 | issue→intent→run→SHA→PR traceability | `run_requirements.intent_id` + `issue_snapshot` JSONB freeze exactly what Jira said at dispatch; the packet's `requirement_hash` lands in `task_run_events.metadata`. |
| NFR-REL-001 | No lost state on crash | Requirement row + baseline row + outbox events are written in one tx each; extraction and baseline are both idempotent (re-run replaces nothing — rows are keyed by run + version). |
| NFR-OPS-001 | Observable | `run.requirement_ready.v1` / `run.baseline_recorded.v1` events; dashboard `RunDetailView` surfaces AC count + baseline status (VTX-13 §4.2). |
| UC-04 | Coding + independent QA journey | This is the first half of UC-04: dispatch-ready evidence exists before the agent runs; VTX-9 consumes the same packet so QA judges *the requirement*, not the agent's summary of it. |

---

## 3. Data Model / DDL Deltas

```sql
-- 00006_requirements.sql — frozen requirement snapshot per run.
CREATE TABLE IF NOT EXISTS run_requirements (
    run_id UUID PRIMARY KEY REFERENCES task_runs(run_id) ON DELETE CASCADE,
    intent_id UUID NOT NULL REFERENCES execution_intents(intent_id),
    jira_site_id TEXT NOT NULL,
    issue_id TEXT NOT NULL,
    issue_key TEXT NOT NULL,
    requirement_version TEXT NOT NULL,          -- 'vtx-req/v1'
    requirement_hash TEXT NOT NULL,             -- sha256 of canonical packet
    summary TEXT NOT NULL,
    acceptance_criteria JSONB NOT NULL,         -- string[] normalized ACs
    issue_snapshot JSONB NOT NULL,              -- raw-ish fetched fields, secrets stripped
    fetched_at TIMESTAMPTZ NOT NULL,            -- Jira re-fetch time
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

Baseline **reuses VTX-8's `verification_runs`** (`kind='baseline'`,
`subject_commit_sha=base_sha`, `status` ∈ `passed|failed|inconclusive`) — one
table for all executed evidence keeps the VTX-8/9/10 reads simple. No second
baseline table.

---

## 4. Go Package & API Design

```
internal/requirement/
  extractor.go   // RequirementExtractor: FetchIssue → normalize → persist
  packet.go      // RequirementPacket struct + canonical hash
  baseline.go    // BaselineRecorder: run suite on base_sha → verification_runs
  dispatch.go    // AgentDispatch assembler (the VTX-7 handoff)
  ports.go       // Store + JiraFetcher + SuiteRunner interfaces
```

### 4.1 Extraction rules

- AC sources (ordered): `JIRA_AC_FIELD` custom field → `## Acceptance Criteria`
  heading parse → `- [ ]` checklist items → empty list (**recorded as
  `ac_count=0`, never synthesized** — the gate can still pass on suite
  evidence; QA flags `no_ac` in its verdict context).
- `issue_snapshot` keeps: summary, description (markdown), labels, priority,
  `issuelinks` (`blocks`/`is blocked by`), subtask keys, `fixVersions`.
  Credentials/comments bodies are excluded (NFR-SEC-001).
- `requirement_hash` = sha256 over canonical JSON (sorted keys) — changes on
  re-extract produce a *new requirement_version row*? No: one row per run (PK);
  a later re-extract for the same run is only allowed while `state='preparing'`
  and overwrites atomically, bumping nothing — the hash in the audit metadata
  is what downstream pins to.

### 4.2 Baseline rules

- Suite command: `BASELINE_CMD` (default `make test`) run **in the run's
  worktree** by VTX-8's `Runner` (same exec boundary, same output capture).
- `base_sha` is the worktree's recorded base (VTX-4 `worktrees.base_sha`).
- Baseline result semantics: `failed` baseline does **not** block dispatch —
  it marks `baseline_status='failed'` in `AgentDispatch` and QA treats
  agent-suite regressions as the delta (STAKEHOLDER S-6.2 on hard-blocking).
- Timeout: `BASELINE_TIMEOUT` (default 15m); timeout ⇒ `status='inconclusive'`.

### 4.3 `AgentDispatch` (handoff contract → VTX-7)

```json
{
  "run_id": "uuid", "attempt": 1,
  "worktree_path": "…", "branch": "vtx/run-<id>", "base_sha": "…",
  "requirement": {
    "issue_key": "VTX-3", "summary": "…",
    "acceptance_criteria": ["…"],
    "requirement_version": "vtx-req/v1", "requirement_hash": "…"
  },
  "baseline": {
    "suite": "make test", "status": "passed|failed|inconclusive",
    "passed": 41, "failed": 0, "artifact_ids": ["…"]
  }
}
```

Matches lane-B's noted dependency (D7-12 prompt-format contract): this
document **is** the VTX-6 side of that contract; the IPC transport shape stays
in `contract-drafts/vtx7-8-agent-ipc.md`.

---

## 5. State Machine Interactions (SRS §5)

| Point | Behavior |
|---|---|
| `queued→preparing` | Scheduler (VTX-5); requirement/baseline work happens inside `preparing`. |
| `preparing→coding` guard | `operational:true` **only when** `run_requirements` row exists AND a `verification_runs(kind='baseline')` row for `(run_id, base_sha)` exists. Else `ErrEvidenceInvalid` (409) — never `ErrStageNotOperational` once VTX-6 ships (VTX-10's guard seam). |
| Baseline infra failure | `block(actor=policy, error_code='baseline_infra')` or `schedule_retry` per VTX-11 classification; baseline suite failure itself is *data*, not a transition error. |
| Jira unreachable at extract | Bounded retry (same policy as intake) → `block` with `error_code='jira_fetch_failed'`; operator resumes (re-extract). |

---

## 6. Error & Retry Semantics

| Failure | Class | Handling |
|---|---|---|
| Jira 401/403 | config | `block` (policy), `jira_auth` |
| Jira 404/410 (issue gone) | permanent | `block`, `jira_issue_gone` — operator decides cancel |
| Jira 5xx/network | retryable | backoff via VTX-11 defaults; exhaustion → `block` |
| Baseline cmd exit≠0 | data | `status='failed'` recorded; dispatch proceeds flagged |
| Baseline timeout/OOM | infra | `status='inconclusive'` + `schedule_retry` (budget 2) |
| Missing worktree row | invariant violation | `block`, `worktree_missing` |

---

## 7. Test Matrix

| ID | Scenario | Type | Covers |
|---|---|---|---|
| REQ-01 | Issue with AC field ⇒ `ac_count` correct, hash stable across re-parse | unit | FR-AGT-001 |
| REQ-02 | Issue with no AC source ⇒ `ac_count=0` persisted, no synthesis | unit | §4.1 |
| REQ-03 | Jira 401 ⇒ `block` + `jira_auth`; 500×3 ⇒ `block` after budget | fake HTTP | §6 |
| REQ-04 | Baseline on known-green fixture ⇒ `verification_runs` row `passed`, `subject_commit_sha=base_sha` | integ | FR-AGT-001 |
| REQ-05 | Baseline RED ⇒ row `failed`, `AgentDispatch.baseline.status='failed'` | integ | §4.2 |
| REQ-06 | `preparing→coding` with no requirement row ⇒ 409 `ErrEvidenceInvalid`; with both rows ⇒ allowed | integ | FR-REG-007 |
| REQ-07 | `AgentDispatch` JSON validates against this contract + lane-B schema | contract | §4.3 |
| REQ-08 | Crash between extract and baseline ⇒ re-entry replays idempotently | fault | NFR-REL-001 |

---

## 8. Open Decisions

| ID | Question | Disposition |
|---|---|---|
| S-6.1 | AC source precedence / custom-field name per Jira tenant? | **STAKEHOLDER** (OD-01) — `JIRA_AC_FIELD` env with the §4.1 fallback chain. |
| S-6.2 | Should a RED baseline block dispatch? | **STAKEHOLDER** — default: proceed flagged; the gate compares deltas. |
| S-6.3 | `BASELINE_CMD` per-repo vs global? | **DECIDED** — global env for MVP; per-repo override is additive config. |
| S-6.4 | Re-extract requirement if issue edited mid-run? | **DECIDED** — no; the packet is frozen at dispatch (audit property). New intents get new runs. |
