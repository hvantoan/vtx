# VTX-5 — Scheduler / Admission / Lease Allocation — Design Specification

**Document status:** Draft for implementation (design lane A, coordinator-written)
**SRS:** `docs/VTX_Platform_SRS_v0.1.docx` (VTX-SRS-001 v0.1)
**Scope:** admission of `queued` runs into the pipeline with a hard cap of 3
concurrent coding executions, durable lease grant/renew/fencing, deferred-intent
promotion, worker heartbeat tracking, and crash recovery.
**Consumes:** `task_runs` (VTX-3), `deferred_intents` (VTX-3), lease columns +
`AcquireRunLease`/`RenewRunLease`/`ReleaseRunLease` (VTX-4 queries),
`outbox_events` `task_run.created.v1` / `task_run.state_changed.v1`.
**Produces:** lease writes on `task_runs`, `worker_heartbeats` rows, outbox
events `task_run.admitted.v1`, `deferred_intent.promoted.v1`,
`run.lease_lost.v1`; the `queued→preparing` transition already marked
`operational: true` is exercised end-to-end.
**Prior input:** `plans/reports/advise-261022-1742-vtx-5-scheduler-preview.json`.

---

## 1. Overview

VTX-5 turns the VTX-3 contract-only scheduler actor into a real component with
four duties:

1. **Admission.** Pick `queued` runs in FIFO order and move them
   `queued --begin--> preparing` while never exceeding **3 concurrently
   admitted coding executions** (FR-SCH-001). Admission is a single DB
   transaction: slot check → lease grant → state transition → audit + outbox.
2. **Lease lifecycle.** Grant a fencing lease (monotonic `lease_epoch`,
   `lease_owner`, `lease_expires_at`) at admission, renew it via worker
   heartbeats, release it on confirmed stop, and fence stale holders through
   the existing VTX-3/4 queries (FR-SCH-002).
3. **Deferred promotion.** When a run leaves the active set, drain the task's
   `deferred_intents` queue FIFO and admit the promoted intent as a new
   `queued` run (FR-REG-004/008).
4. **Recovery.** Detect expired leases / lost heartbeats, park the run in
   `blocked` (actor `policy`), hold its slot until the agent is confirmed
   stopped, and let an operator resume/cancel with audit (FR-SCH-004, UC-06).

The scheduler does **not** allocate worktrees (VTX-4 `WorktreeService` does
that inside `preparing`), does not run agents (VTX-7), and does not judge
evidence (VTX-9/10). It only decides *who may hold a coding slot*.

---

## 2. SRS Coverage Table

| SRS ID | Requirement (paraphrased) | Design decision |
|---|---|---|
| FR-SCH-001 | Admit ≤3 coding tasks concurrently; queue overflow policy published | `AdmissionLedger` counts **live slots** = nonterminal runs whose `lease_expires_at > now` OR whose state ∈ {`preparing`,`coding`} OR blocked-with-unconfirmed-stop (§4.3). Cap is a DB-enforced invariant under `pg_advisory_xact_lock`, not an in-memory counter. Overflow runs stay `queued` (FIFO); policy is published via `GET /operator/overview.admission` (VTX-13). |
| FR-SCH-002 | Lease owner + monotonic epoch + expiry + renew; stale worker fenced | Reuse VTX-4 queries verbatim: `AcquireRunLease` (epoch++ CAS on locked row), `RenewRunLease` (owner+epoch+unexpired match), `ReleaseRunLease` (owner+epoch match). Every mutating call downstream carries `expected_lease_epoch`; the domain `checkLease` already rejects stale/foreign leases (§4.4). |
| FR-SCH-003 | Per-run worktree created/validated/cleaned; no cross-run reuse | Worktree allocation happens inside `preparing` via VTX-4 `WorktreeService.Allocate(runID)` — scheduler only triggers `begin`; isolation guarantees stay in VTX-4 (§5). |
| FR-SCH-004 | Crash recovery keeps ownership clear; cleanup never frees a live-leased worktree | `RecoverySweeper` classifies runs: expired lease + nonterminal ⇒ `block` (actor `policy`, `error_code=lease_lost`) and **slot stays counted** until stop confirmed; worktree reconciler (VTX-4) already refuses release while `run_lease_expires_at > now` (§6). |
| FR-REG-004 | ≥1 admitted nonterminal run per task; intents deferred durably | `deferred_intents` rows already serialize per task. `DeferredPromoter` drains FIFO `WHERE disposition='pending_deferred'` for tasks with no active run, flips `admitted`, creates the new `queued` run + `task_run.created.v1` in one tx (§4.5). |
| FR-REG-005 | CAS + actor authz + lease fencing on every transition | Scheduler mutations go through `RegistryTransitioner.TransitionRun` exactly like VTX-3: `expected_version`, `expected_lease_epoch`, actor `scheduler` for `begin`/`retry_due` (§4.1). |
| FR-EVT-001/004 | Consumer routing by event_type; no phantom ACKs | Scheduler consumes `task_run.created.v1` + `task_run.state_changed.v1` via a second `Dispatcher` consumer name `scheduler`; registry's consumer never ACKs them (existing per-consumer receipts) (§4.2). |
| UC-03 | Run admitted or deferred per capacity | `admitted` vs `deferred` is decided at intake (VTX-3) by active-run presence; VTX-5 decides *queued→preparing* by slot availability — two different gates, both durable (§1). |
| UC-06 | Crash loses no state; stale lease rejected | Admission is one tx; a mid-tx crash leaves the run `queued` and is re-picked on the next tick (idempotent because `InsertTaskRunEvent`/outbox dedupe) (§6). |
| NFR-CON-001 | ≥2 writer processes, exactly one wins | `pg_advisory_xact_lock(hashtext('vtx-admission'))` serializes the slot-count check across all replicas; CAS on `task_runs.version` remains the second line (§4.3). |
| NFR-OPS-001 | Observable admission/lag | `task_run.admitted.v1` carries `live_slots`; `GET /operator/overview` already exposes `admission:{max,active}` (VTX-13); `worker_heartbeats` rows feed `/operator/workers`. |
| NFR-REL-001 | No lost intent/run on crash | Every mutation is inside one pgx tx; admission, promotion, and block-on-lease-loss are all idempotent replays (§6). |
| NFR-AUD-001 | Traceable actor/reason | `task_run_events` rows carry actor=`scheduler`/`policy`, `correlation_id` propagated from run; `run.lease_lost.v1` records the fencing evidence. |

---

## 3. Data Model / DDL Deltas (future migration `00011_scheduler.sql`)

```sql
-- Heartbeat registry for scheduler workers and agent hosts (VTX-7) —
-- feeds GET /api/v1/operator/workers (VTX-13 FR-UI-001) and the
-- RecoverySweeper's "is anyone still alive" classification.
CREATE TABLE IF NOT EXISTS worker_heartbeats (
    -- Reconciled 2026-10-25 with vtx-13 §3: composite PK lets one host run
    -- several kinds; run_id/queue/active_runs serve both the sweeper and
    -- the /operator/workers view.
    worker_id TEXT NOT NULL,             -- e.g. 'scheduler-01', 'agent-host-<run>'
    worker_kind TEXT NOT NULL CHECK (worker_kind IN
        ('temporal','dispatcher','scheduler','agent_host','sweeper')),
    run_id UUID REFERENCES task_runs(run_id) ON DELETE SET NULL,  -- agent_host only
    queue TEXT,
    active_runs INT NOT NULL DEFAULT 0,
    last_beat_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    metadata JSONB,                      -- version, host, pid, capacity notes
    started_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (worker_id, worker_kind)
);
CREATE INDEX IF NOT EXISTS idx_worker_heartbeats_beat
    ON worker_heartbeats (last_beat_at);

-- A worker is 'up' when now() - last_beat_at < 3 * beat interval (default 15s).
-- Computed at read time — no status column, per VTX-13 "measured, not hardcoded".
```

No other new tables: the admission ledger is **derived** from `task_runs`
(§4.3) so there is no second source of truth to reconcile.

Required query additions (`db/queries/scheduler.sql`):

```sql
-- name: ListQueuedRunsForAdmission :many
SELECT run_id, task_id, version FROM task_runs
WHERE state = 'queued'
ORDER BY created_at, run_id
LIMIT $1 FOR UPDATE SKIP LOCKED;   -- replicas grab distinct candidates

-- name: CountLiveAdmissionSlots :one
-- The cap counts real concurrency, not queue depth.
SELECT COUNT(*) FROM task_runs
WHERE state NOT IN ('completed','failed','cancelled')
  AND (
    state IN ('preparing','coding')
    OR (lease_expires_at IS NOT NULL AND lease_expires_at > CURRENT_TIMESTAMP)
    OR (state = 'blocked' AND error_code = 'lease_lost')   -- unconfirmed stop
  );

-- name: ListExpiredLeaseRuns :many  (RecoverySweeper input)
SELECT run_id, version, lease_owner, lease_epoch, lease_expires_at, state
FROM task_runs
WHERE state NOT IN ('completed','failed','cancelled')
  AND lease_expires_at IS NOT NULL
  AND lease_expires_at < CURRENT_TIMESTAMP
LIMIT $1;

-- name: ClaimDeferredForTask :one  (promotion, FIFO head of line)
UPDATE deferred_intents SET disposition = 'admitted'
WHERE intent_id = (
  SELECT intent_id FROM deferred_intents
  WHERE task_id = $1 AND disposition = 'pending_deferred'
  ORDER BY received_at, intent_id LIMIT 1 FOR UPDATE SKIP LOCKED
)
RETURNING intent_id, task_id;

-- name: UpsertWorkerHeartbeat :one
INSERT INTO worker_heartbeats (worker_id, worker_kind, run_id, last_beat_at, metadata)
VALUES ($1,$2,$3,CURRENT_TIMESTAMP,$4)
ON CONFLICT (worker_id) DO UPDATE
SET last_beat_at = CURRENT_TIMESTAMP, run_id = EXCLUDED.run_id,
    metadata = EXCLUDED.metadata
RETURNING worker_id;
```

---

## 4. Go Package & API Design

```
internal/scheduler/
  scheduler.go     // SchedulerService: main loop + admission path
  admission.go     // AdmissionLedger + Admit() tx (slot check → lease → begin)
  promotion.go     // DeferredPromoter: drain FIFO on terminal transition
  heartbeat.go     // Heartbeat writer (scheduler workers) + staleness reader
  sweeper.go       // RecoverySweeper: expired leases → block via policy actor
  ports.go         // Store interfaces (below)
```

### 4.1 Ports (`internal/scheduler/ports.go`)

```go
type SchedulerStore interface {
    ListQueuedRunsForAdmission(ctx, limit int) ([]QueuedRun, error)
    CountLiveAdmissionSlots(ctx) (int, error)
    AcquireRunLease(ctx, runID, owner string, expiresAt time.Time) (Lease, error)  // VTX-4 query
    TransitionRun(ctx, TransitionCmd) (Run, error)                                  // registry CAS path
    ListExpiredLeaseRuns(ctx, limit int) ([]RunLease, error)
    ClaimDeferredForTask(ctx, taskID uuid.UUID) (DeferredClaim, error)
    HasActiveRunForTask(ctx, taskID uuid.UUID) (bool, error)
    CreateRunForIntent(ctx, intentID uuid.UUID) (Run, error)                        // wraps VTX-3 consume path
    UpsertWorkerHeartbeat(ctx, beat Heartbeat) error
}
```

All called inside one `pgx.Tx` per admission/promotion — same boundary
discipline as VTX-3/4.

### 4.2 Trigger model

- **Event-driven fast path:** dispatcher consumer `scheduler` on
  `task_run.created.v1` (new queued run → try admit) and
  `task_run.state_changed.v1` (terminal transition → promote deferred for that
  task, then try admission).
- **Tick fallback:** `SCHEDULER_TICK` (default 5s) sweeps the same two
  routines — the events are a latency optimization, never a correctness input.
- **Beats:** each scheduler replica writes `worker_heartbeats`
  (worker_id='scheduler-<n>', kind='scheduler')` every `BEAT_INTERVAL` (15s);
  `agent_host` beats are written by VTX-7 keyed to `run_id`.

### 4.3 Admission tx (the ≤3 invariant)

```text
BEGIN
  pg_advisory_xact_lock(hashtext('vtx-admission'))   -- serialize slot math
  live := CountLiveAdmissionSlots()
  WHILE live < 3 AND candidate := next from ListQueuedRunsForAdmission(3-live):
     row := GetTaskRunForUpdate(candidate.run_id)
     IF eligibility(candidate) == false: CONTINUE   -- §4.6
     lease := AcquireRunLease(row, owner='scheduler-<id>', ttl=LEASE_TTL)
     TransitionRun(begin→preparing, actor=scheduler, expected_version,
                   expected_lease_epoch=lease.epoch)
     emit task_run.admitted.v1 {run_id, lease_epoch, live_slots: live+1}
     live++
COMMIT
```

- `pg_advisory_xact_lock` makes the count+grant atomic across replicas;
  `SKIP LOCKED` on candidates means two replicas never fight over the same run.
- `begin` requires a lease (`checkLease`), so **lease grant precedes the
  transition inside the same tx** — order is load-bearing.

### 4.4 Lease policy

| Parameter | Default | Notes |
|---|---|---|
| `LEASE_TTL` | 120s | Grant at admission; VTX-7 host renews every 15s during execution. |
| `BEAT_INTERVAL` | 15s | Scheduler + agent-host heartbeat cadence; `up` = beat < 3× interval. |
| `LEASE_TTL` vs work | TTL covers agent runtime; renews keep it alive; terminal/blocked transitions release via `ReleaseRunLease` owner+epoch. |

Stale-lease rejection is already structural: `RenewRunLease` returns 0 rows for
wrong owner/epoch/expired; downstream `TransitionRun` calls carry
`expected_lease_epoch` and the domain rejects mismatch — VTX-5 changes nothing
in that contract, it is simply the first writer that *grants* leases (VTX-4
granted them only inside worktree allocation before).

### 4.5 Deferred promotion

```text
on task_run.state_changed.v1 to terminal (or sweeper tick):
  FOR EACH task whose run went terminal:
    IF HasActiveRunForTask(task): continue
    claim := ClaimDeferredForTask(task)           -- FIFO head, one at a time
    IF claim:
      run := CreateRunForIntent(claim.intent_id)   -- state='queued', v=0
      emit task_run.created.v1 + deferred_intent.promoted.v1
```

Promotion creates a **new** run for the new intent (the old run stays terminal
history) — `deferred_intents.admitted` marks consumption; inbox receipt already
guarantees the intent's single persisted effect.

### 4.6 Eligibility (DECIDED, config-gated)

`SCHEDULER_JIRA_GUARD` (default `off`): when `on`, admission first calls
`IssueFetcher.FetchIssue` and skips (stays `queued`) if the issue is no longer
eligible or a `blocked by` link is unresolved — "can't read Jira ⇒ don't
admit". Default `off` because Jira reachability in the MVP network is an open
decision (OD-01); the flag makes the guard a deployment choice, not a hidden
behavior.

---

## 5. State Machine Interactions (SRS §5)

| Hop | Actor | Note |
|---|---|---|
| `queued --begin--> preparing` | `scheduler` | Only inside admission tx after lease grant. `operational:true` already. |
| `preparing --advance--> coding` | `orchestrator` | Flipped operational by VTX-6/8 readiness (baseline + worktree + agent dispatch). Not VTX-5's flip. |
| `* --schedule_retry--> retry_wait` | `policy` | Driven by VTX-11 policy; scheduler does not decide budgets. |
| `retry_wait --retry_due--> resume` | `scheduler`,`policy` | Scheduler sweeps `retry_due_at <= now` rows on its tick. |
| `* --block--> blocked` | `operator`,`policy` | Sweeper uses `policy` for lease-loss; operator uses dashboard. |
| `* --cancel/fail` | `operator`,`policy` | Releases the slot: terminal states never count (§3 query). |

**Slot accounting (prior advise, adopted):** a slot is held from lease grant
until the run is terminal **and** the coding agent is confirmed stopped.
`blocked` with `error_code='lease_lost'` keeps counting (the agent may still be
running — conservative per FR-SCH-004); the operator's `cancel` (or a confirmed
agent-exit event from VTX-7) frees it. Downstream stages
(`verifying`,`qa`,`quality_gate`,`pr_draft`) hold a lease for fencing but the
advise's "only coding occupies a slot" is **narrowed** in this spec for MVP
safety: any nonterminal run with a live lease counts, so the cap is a ceiling
on *concurrent pipeline executions*, never exceeded (STAKEHOLDER flag S-5.1
notes the looser coding-only alternative).

---

## 6. Error & Retry Semantics

| Failure | Handling |
|---|---|
| Admission tx crash mid-commit | Rollback ⇒ run stays `queued`; next tick re-admits. Idempotent: lease/epoch bump is inside the tx. |
| Scheduler replica crash | `SKIP LOCKED` releases candidates; heartbeats go stale → `/operator/workers` shows `down`; other replica continues. |
| Agent worker dies mid-coding | `lease_expires_at` lapses ⇒ sweeper `block(actor=policy, error_code='lease_lost')` + `run.lease_lost.v1`; slot held until operator cancel or VTX-7 agent-exit confirmation. |
| `retry_due_at` due | Sweeper `TransitionRun(retry_due, actor=scheduler)` → resumes into `resume_state` (VTX-3 contract). |
| Deferred promotion crash | `ClaimDeferredForTask` + `CreateRunForIntent` in one tx ⇒ atomic; replay re-claims nothing because `disposition` flipped inside the tx. |
| Poison candidate (transition keeps failing) | Not retried silently: run stays `queued` and the error is logged with `correlation_id`; 3 consecutive failed admissions for the same run ⇒ `block` with `error_code=admission_failed` (bounded, FR-ERR-001). |

---

## 7. Test Matrix (SRS §9 evidence)

| ID | Scenario | Type | Covers |
|---|---|---|---|
| SCH-01 | 5 queued runs ⇒ exactly 3 admitted, 2 stay `queued`; `live_slots` in event = 3 | pgx integ | FR-SCH-001, E2E-06 |
| SCH-02 | Two replicas race admission of 6 runs ⇒ live count never > 3, no duplicate `task_run_events.version` | 2-process integ | NFR-CON-001, FR-SCH-001 |
| SCH-03 | `begin` without lease ⇒ `ErrStaleLease`; begin with lease ⇒ `preparing` | unit | FR-REG-005, §5 |
| SCH-04 | Epoch bump: stale worker's `RenewRunLease(old_epoch)` ⇒ 0 rows; its `TransitionRun` ⇒ `ErrStaleLease` | integ | FR-SCH-002 |
| SCH-05 | Let lease lapse ⇒ sweeper blocks run (`policy`), `run.lease_lost.v1` emitted, slot still counted | integ | FR-SCH-004, UC-06 |
| SCH-06 | Active run on task + new intent ⇒ deferred; run completes ⇒ promoter creates new `queued` run in FIFO order | integ | FR-REG-004, E2E-02-ish |
| SCH-07 | Crash-simulated mid-admission (kill before commit) ⇒ run `queued`, no partial lease/events | fault inject | NFR-REL-001 |
| SCH-08 | `retry_due_at` past ⇒ sweeper `retry_due` resumes correct `resume_state` | integ | §5 |
| SCH-09 | `worker_heartbeats` staleness ⇒ `/operator/workers` reports `down` after 3× interval | integ | FR-UI-001 |
| SCH-10 | `SCHEDULER_JIRA_GUARD=on` + FetchIssue fail ⇒ run not admitted; `=off` ⇒ admitted | unit+fake | §4.6 |

---

## 8. Open Decisions

| ID | Question | Disposition |
|---|---|---|
| S-5.1 | Slot = any live-lease nonterminal run (this spec, conservative) vs coding-stage only (prior advise)? | **STAKEHOLDER** — recommend conservative for MVP; relaxing later is additive. |
| S-5.2 | `SCHEDULER_JIRA_GUARD` default and whether `blocked by` links block admission? | **STAKEHOLDER** (OD-01 dependent); shipped `off`. |
| S-5.3 | Lease TTL for downstream (non-coding) stages — same 120s or per-stage? | **DECIDED** — single `LEASE_TTL`, renew cadence identical; revisit with real timings (NFR-PERF-001 TBD). |
| S-5.4 | Scheduler placement: in-process service in `cmd/worker` vs Temporal workflow? | **DECIDED** — in-process loop in `cmd/worker`, independent lifecycle per FR-EVT-006; Temporal orchestration stays a later phase (SRS §2.2). |
| S-5.5 | Admission priority beyond FIFO (Jira priority field)? | **DECIDED** — FIFO for MVP; priority ordering is additive (`ORDER BY` change only). |
