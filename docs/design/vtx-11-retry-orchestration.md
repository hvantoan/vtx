# VTX-11 — Bounded Retry & Error Orchestration Design Spec

**Document status:** Draft for implementation (design lane C)
**SRS:** `docs/VTX_Platform_SRS_v0.1.docx` (VTX-SRS-001 v0.1)
**Scope:** FR-ERR-001 — failures retry within a bounded policy, never loop
forever, and terminate with evidence. Covers stage-failure classification,
`schedule_retry`/`retry_due` policy, `blocked`/`resume` operator flow,
crash-recovery sweeping, and outbox dead-letter handling.
**Consumes:** `task_runs.attempt`/`retry_due_at`/`resume_state`/`error_code`
(00004), the existing `block|schedule_retry|retry_due|resume|fail|cancel`
verbs (domain `Evaluate`), outbox DLQ columns (00004).
**Produces:** `internal/retry` package, `failure_class`/`retry_policy_version`
columns (00008), operator redrive API, audit metadata conventions.

---

## 1. Overview

The VTX-3 state machine already provides the *mechanics*: `schedule_retry`
parks a run in `retry_wait` with `resume_state` set and `attempt+1`;
`retry_due` (gated on `retry_due_at <= now`) returns it to `resume_state`;
`fail`/`cancel` terminate; `block`/`resume` give the operator a parking brake.
What is missing — and what this spec adds — is the **policy layer** that
decides *whether* and *when* those verbs fire:

1. **Classification.** Every failure observed by the pipeline driver is mapped
   to a `FailureClass` (§4.2) which determines retryability. Classification is
   a pure function of the error + run context — never an LLM call.
2. **Budget.** A versioned retry policy (`vtx-retry-policy/v1`) bounds attempts
   per run per failure class. Budget exhaustion or a non-retryable class yields
   `fail` → terminal `failed` **with evidence**: the full `task_run_events`
   history plus the decision metadata recorded on the final transition.
3. **Scheduling.** `retry_due_at = now + backoff(attempt)` with exponential
   backoff + jitter, replacing VTX-3's placeholder fixed `defaultRetryDelay`
   (the registry comment already marks it as VTX-5/VTX-11's decision).
4. **Recovery.** A sweeper reconciles runs stuck in nonterminal states with
   expired leases (worker crash, E2E-03) into `retry_wait` or `blocked`
   per class — never silently, always with an audit row.
5. **Blocked/resume.** `blocked` is the human-attention state: only
   `operator` may `resume`; `resume` targets the stored `resume_state`
   (already enforced by `domain.Evaluate`).

No new verbs, no new states: VTX-11 is orchestration *around* the existing
machine, which keeps the §5 contract table intact.

---

## 2. SRS Coverage Table

| SRS ID | Requirement (paraphrased) | Design decision |
|---|---|---|
| FR-ERR-001 | Bounded retry, no infinite loops, terminal failure + evidence | `RetryPolicy` caps attempts per failure class (default `max_attempts=3` per run); exhaustion/non-retryable ⇒ `fail` with `error_code` + audit metadata `retry_policy_version`, `failure_class`, `attempts_used` (§4.3). |
| FR-REG-006 | `blocked`/`retry_wait` resumable; `failed`/`cancelled`/`completed` terminal | Resume paths unchanged: `blocked --resume--> resume_state` (operator), `retry_wait --retry_due--> resume_state` (scheduler/policy). Policy only *chooses* among existing verbs. |
| FR-EVT-002 | Bounded claim, lease expiry, retry/backoff, dead letter, audited redrive | Outbox dispatcher already claims/retries/DLQs; VTX-11 adds the redrive endpoint + `outbox_events` redrive audit (`metadata` on a new outbox row is not needed — redrive updates `dead_lettered_at=NULL` and writes a `delivery_redrives` row, §3). |
| FR-EVT-003 | ACK only after consumer commit; crash ⇒ replay, no dup effect | Unchanged contract; the retry layer treats dispatcher redelivery as the trigger, consumer receipts (`inbox_consumptions`) absorb duplicates. |
| FR-SCH-002/004 | Stale worker fenced; crash recovery with clear ownership | Recovery sweeper only schedules transitions; `domain.Evaluate` still enforces lease/epoch fencing — a dead worker holding a stale lease cannot write (§5). |
| FR-UI-003 | Operator mutations need authenticated principal + audit | Redrive/resume endpoints require `operator` principal; the transition path already audits actor/reason/correlation. |
| NFR-REL-001 | Crash ⇒ no lost intent/state; safe replay | All retry decisions are issued through `TransitionRun` (single tx: state+audit+outbox); the sweeper is idempotent — it selects runs by predicate, and CAS makes a second sweeper pass a no-op (§6). |
| NFR-CON-001 | Concurrent writers ⇒ one commit | Sweeper and orchestrator both go through `TransitionRun`; `stale_version` resolves the race (§6). |
| NFR-AUD-001 | Traceability, no secrets | `failure_class`, `retry_policy_version`, `attempt`, `backoff_ms`, `next_due_at` recorded in `task_run_events.metadata`; `error_message` sanitized by existing `sanitizeReason`. |
| NFR-OPS-001 | Observable retries/DLQ/active runs | `retry_wait` runs queryable by `retry_due_at`; DLQ depth via existing `OutboxLag`; new `delivery_redrives` table exposes redrive history. |
| UC-06 | Retry/cancel/crash recovery journey | §5 maps every UC-06 branch to a concrete actor+verb+policy path. |

---

## 3. Data Model / DDL Deltas (migrations 00006+)

```sql
-- 00008_retry_orchestration.sql — retry classification + redrive audit.

-- Queryable failure classification on the run (audit metadata also carries it,
-- but columns make ops queries cheap: "all runs failed with lease_lost").
ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS failure_class TEXT;
ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS retry_policy_version TEXT;

-- Redrive audit for outbox dead letters (FR-EVT-002 "redrive có audit").
-- A redrive = clear dead_lettered_at/dead_letter_reason on the outbox row and
-- record who did it and why.
CREATE TABLE IF NOT EXISTS delivery_redrives (
    redrive_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    outbox_event_id UUID NOT NULL,          -- no FK: retention must not cascade
    consumer_name VARCHAR(64) NOT NULL,
    actor VARCHAR(64) NOT NULL,             -- operator principal
    actor_type TEXT NOT NULL DEFAULT 'human',
    reason TEXT NOT NULL,
    correlation_id VARCHAR(128),
    redriven_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_delivery_redrives_event
    ON delivery_redrives (outbox_event_id);

-- Sweeper scan support: nonterminal runs whose lease expired.
CREATE INDEX IF NOT EXISTS idx_task_runs_recovery
    ON task_runs (lease_expires_at)
    WHERE state NOT IN ('completed', 'failed', 'cancelled');
```

`task_run_events` is unchanged — retry decisions ride the existing `metadata`
JSONB (convention keys: `failure_class`, `retry_policy_version`,
`backoff_ms`, `attempts_used`, `attempts_max`, `resume_state`).

---

## 4. Go Package & API Design

New package: `apps/backend/internal/retry`. No new transition actors — the
policy drives transitions through `RegistryService` as actor `policy` (already
authorized for `schedule_retry`, `retry_due`, `fail`, `cancel`, `block`).

### 4.1 Types

```go
// FailureClass is the retryability taxonomy. Wire-stable strings — they are
// stored on task_runs.failure_class and in audit metadata.
type FailureClass string
const (
    ClassAgentCrashed     FailureClass = "agent_crashed"      // process died/timeout — retryable
    ClassLeaseLost        FailureClass = "lease_lost"         // stale fence detected — retryable
    ClassWorktreeLost     FailureClass = "worktree_lost"      // fs corruption — retryable after realloc
    ClassInfraTransient   FailureClass = "infra_transient"    // db/network blip — retryable
    ClassEvidencePending  FailureClass = "evidence_pending"   // gate evidence not yet arrived — retryable
    ClassVerificationFail FailureClass = "verification_failed"// tests red — functional
    ClassQAVerdictFail    FailureClass = "qa_verdict_fail"    // QA says bad — functional
    ClassGateFail         FailureClass = "gate_fail"          // mandatory condition unmet — functional
    ClassNonRetryable     FailureClass = "non_retryable"      // policy violation, poison payload
    ClassOperatorBlocked  FailureClass = "operator_blocked"   // parked for human, not a failure
)

// RetryPolicy (vtx-retry-policy/v1) — loaded from config, snapshotted onto
// the run at first schedule_retry for audit stability.
type RetryPolicy struct {
    Version       string                    // 'v1'
    MaxAttempts   int32                     // per-run attempt budget (default 3)
    BaseDelay     time.Duration             // default 30s
    MaxDelay      time.Duration             // default 10m
    BackoffFactor float64                    // default 2.0
    JitterFrac    float64                    // default 0.2 (±20%)
    ClassAction   map[FailureClass]Disposition // retry | fail | block
}
```

`Disposition`: `retry` (schedule_retry), `fail` (terminal), `block` (park for
operator). Defaults: retryable classes → `retry`; functional classes → `fail`;
`evidence_pending` → `retry` with its own sub-cap (same budget);
`non_retryable` → `block` (evidence is preserved for the operator rather than
auto-failing — human judgment, §8 D-11.4).

### 4.2 Ports

```go
type RetryService interface {
    // Decide classifies the failure and returns the transition the policy
    // wants. Pure decision — the caller (pipeline driver/sweeper) executes it
    // through RegistryService so CAS/lease fencing still applies.
    Decide(ctx context.Context, run RunSnapshot, cause error, cls FailureClass) (RetryDecision, error)
    // DueForRetry lists retry_wait runs whose retry_due_at <= now (sweep).
    DueForRetry(ctx context.Context, limit int) ([]RunSnapshot, error)
    // ExpiredLeaseRuns lists nonterminal runs whose lease_expires_at < now.
    ExpiredLeaseRuns(ctx context.Context, limit int) ([]RunSnapshot, error)
}

// Classifier maps an observed error to a FailureClass. Pure function.
type Classifier interface {
    Classify(err error, run RunSnapshot) FailureClass
}

type RetryStore interface {
    ListRetryWaitDue(ctx context.Context, now time.Time, limit int) ([]RunSnapshot, error)
    ListExpiredLease(ctx context.Context, now time.Time, limit int) ([]RunSnapshot, error)
    RedriveDelivery(ctx context.Context, outboxEventID, consumer, actor, reason, correlationID string) error // tx: clear DLQ fields + insert delivery_redrives
}
```

`RetryDecision` = `{action: schedule_retry|fail|block, retry_due_at,
failure_class, policy_version, reason}` → translated into a
`TransitionCommand` (actor=`policy`, `expected_version` from the locked run)
with the metadata keys from §3.

### 4.3 Backoff

```
delay(n) = min(MaxDelay, BaseDelay * BackoffFactor^(n-1)) * (1 ± JitterFrac)
attempts: task_runs.attempt is incremented by the existing schedule_retry plan
budget: allow schedule_retry while run.attempt < MaxAttempts (attempt counts
        retries already taken; attempt 3 with MaxAttempts=3 ⇒ next failure ⇒ fail)
```

On `fail`: `error_code` = `retry_budget_exhausted` | the class name; the
`task_run_events` chain IS the evidence trail (NFR-AUD-001) — the spec adds no
parallel log.

### 4.4 Transport (operator + internal)

| Method | Path | Principal | Purpose |
|---|---|---|---|
| POST | `/internal/v1/outbox/dead-letters/{event_id}/redrive` | `operator` | Audited DLQ redrive → `delivery_redrives` |
| GET  | `/internal/v1/runs?state=retry_wait,blocked` | `operator` | Triage list for dashboard |
| POST | `/internal/v1/runs/{id}/block` `/{id}/resume` `/{id}/cancel` | `operator` | Existing `TransitionRun` verbs — no new endpoint needed, listed for completeness |

### 4.5 Backward-retry question (cross-ref VTX-9 D-9.4)

The §5 table has no backward edge; `retry_wait` resumes to the state it parked
from. Recommended (mirrored in §8 D-11.2): extend `TransitionCommand` with an
optional `RetryTarget` honored only by `schedule_retry` and constrained to
states earlier in `stageAdvance` — enabling e.g. `qa --schedule_retry(target=coding)--> retry_wait` → resume to `coding`. This is a VTX-3 domain amendment
and is held as STAKEHOLDER, not silently assumed.

---

## 5. State Machine Interactions (SRS §5)

| Trigger | Path | Actor | Notes |
|---|---|---|---|
| Stage fails, retryable class, budget left | `X --schedule_retry--> retry_wait` | `policy` | `resume_state=X`, `attempt+1`, `retry_due_at=now+backoff` |
| `retry_due_at` reached | `retry_wait --retry_due--> X` | `scheduler`/`policy` | VTX-5 sweeper or in-process driver calls it |
| Retryable class, budget exhausted | `X --fail--> failed` | `policy` | `error_code=retry_budget_exhausted` |
| Functional fail (QA verdict, gate fail, verification red) | `X --fail--> failed` | `policy` | evidence: verdict/decision rows already persisted |
| Non-retryable / poison | `X --block--> blocked` | `policy` | operator triage; `resume` by `operator` only |
| Worker crash (lease expired, nonterminal) | sweeper → `schedule_retry` (`lease_lost`) or `block` | `policy` | fenced: stale worker can't write (FR-SCH-002) |
| Operator cancel | `X --cancel--> cancelled` | `operator` | terminal; cancelled runs never revive |

`resume_state` integrity stays with `domain.Evaluate` (parked state required,
validated). `retry_wait` runs re-enter their parked stage, so e.g. a `qa`
re-entry requires a fresh verdict for the current attempt (VTX-9 keying).

---

## 6. Error & Retry Semantics

- **Idempotent sweeper.** Both sweeps select by predicate and act via
  `TransitionRun` with `expected_version` from the read row. Two sweeper
  replicas racing ⇒ one wins CAS, the other gets `stale_version` and skips —
  no duplicate effects (FR-EVT-005 pattern, NFR-CON-001).
- **Crash mid-decision.** `Decide` is pure; its result is executed in
  `TransitionRun`'s single tx. Crash before the tx ⇒ run unchanged, next
  sweep/heartbeat retries the decision. No "half-parked" state exists.
- **DLQ semantics.** A delivery is dead-lettered by the dispatcher after
  `MaxAttempts`; redrive clears `dead_lettered_at`/`dead_letter_reason` and
  inserts `delivery_redrives` in one tx. Redrive does not delete DLQ history —
  `attempt` and prior `dead_letter_*` remain visible via the redrive rows.
- **No silent loss.** Every policy outcome is a transition (audited) or a
  logged-and-metered skip (`metric=retry_sweep_skip`, reason). Terminal
  `failed`/`cancelled` runs are excluded from all sweeps by the partial index
  predicate and `IsTerminal()` guard.
- **Jitter is non-deterministic input to scheduling only** — it never enters
  the state machine or gate; determinism requirements (Temporal) are
  unaffected because the delay is computed and persisted, then read back.

---

## 7. Test Matrix (SRS §9 evidence)

Contributes to §9.1 "VTX-4/5" + "VTX-6..12" rows, TR-style registry tests, and
E2E-03/04/06/07.

| ID | Test | SRS evidence |
|---|---|---|
| RT-01 | Unit: `Classify` maps sentinel errors → classes (crash, lease, poison, verification) | FR-ERR-001 |
| RT-02 | Unit: backoff formula bounds — `delay(1)=Base`, `delay(∞)→MaxDelay`, jitter within ±20% | FR-ERR-001 |
| RT-03 | Unit: `Decide` at `attempt < max` → `schedule_retry`; at `attempt == max` → `fail` | FR-ERR-001 bounded |
| RT-04 | Unit: non-retryable class → `block`, never `schedule_retry` | FR-ERR-001 |
| RT-05 | Integration: `schedule_retry` sets `resume_state`, `attempt+1`, `retry_due_at`; `retry_due` before due ⇒ `invalid_transition` | FR-REG-006 (existing domain test coverage extended) |
| RT-06 | Integration: sweeper picks expired-lease nonterminal run → `retry_wait`; terminal run ignored | FR-SCH-004/E2E-03 |
| RT-07 | Integration: two sweeper replicas ⇒ one transition commits, other `stale_version` | NFR-CON-001/FR-EVT-005 |
| RT-08 | Integration: budget exhaustion ⇒ `failed` terminal; audit chain shows every `schedule_retry` + final `fail` with `retry_policy_version` | FR-ERR-001/NFR-AUD-001 |
| RT-09 | Integration: DLQ event → redrive endpoint clears DLQ + writes `delivery_redrives`; second redrive is a no-op | FR-EVT-002/E2E-07 |
| RT-10 | Integration: `blocked --resume--> resume_state` requires operator actor; policy `resume` ⇒ `forbidden` | FR-REG-006/UC-06 |
| RT-11 | E2E (staging): worker killed during `coding` ⇒ recovery sweeper reparks, no stale writes, no duplicate effects | E2E-03 |
| RT-12 | E2E: QA-fail run terminates `failed` with gate/verdict evidence; no Draft PR | E2E-04 |

---

## 8. Open Decisions

| ID | Decision | Status |
|---|---|---|
| D-11.1 | Defaults: `MaxAttempts=3`, `BaseDelay=30s`, `MaxDelay=10m`, factor 2, jitter ±20%, policy id `vtx-retry-policy/v1` stamped on run + audit rows. | **DECIDED (recommended)** — conservative MVP bounds; config-driven so stakeholders tune numbers, not mechanism. NFR-PERF-001 SLOs may later justify tighter caps. |
| D-11.2 | Backward retry (e.g., QA-fail → re-run `coding`) requires a `RetryTarget` extension to `schedule_retry` — a VTX-3 domain amendment. MVP alternative: functional failures terminate; re-entry via a new Jira intent (which creates a fresh run per FR-REG-004). | **STAKEHOLDER** — recommended: keep no-backward-edge for MVP (state machine stays a DAG; audit is linear); revisit if E2E-04's "retries bounded" is ruled to mean coding retries. Owner: SRS author / VTX-3 owner. |
| D-11.3 | Recovery sweeper runs as an in-process loop with `SKIP LOCKED`-style claim (same pattern as `ClaimOutbox`), not a Temporal activity. | **DECIDED (recommended)** — keeps the recovery path independent of Temporal availability (OD-04, FR-EVT-006); a Temporal driver can later invoke `DueForRetry`/`ExpiredLeaseRuns` unchanged. |
| D-11.4 | `non_retryable`/poison ⇒ `block` (park for operator) rather than immediate `fail`. | **DECIDED (recommended)** — preserves evidence for investigation and matches "operator handles DLQ" (UC-07); stakeholder may prefer fail-fast. |
| D-11.5 | `evidence_pending` gate-fail retries share the run's global `MaxAttempts` rather than a separate sub-budget. | **DECIDED** — simpler audit (one counter); a dedicated cap is a policy-map entry if later needed. |
| D-11.6 | Who can `retry_due`: `scheduler` and `policy` (per existing table). Whether the VTX-5 scheduler or an in-process driver performs the sweep is a deployment detail. | **DECIDED** — contract unchanged; deployment choice deferred to VTX-5 owner without contract impact. |
| D-11.7 | `cancel` is operator/policy-only and terminal — cancelled runs never re-enter via retry (must come from a new intent). | **DECIDED** — already the domain table's semantics; restated so no lane "helpfully" adds a revive path. |
