# Coding Slot Scheduler — VTX-5 (AUTO-04)

Owns admission for **at most 3 concurrent coding-agent slots** across the
whole system — all projects, all workers, all scheduler replicas share one
pool. PostgreSQL is the only source of truth; there is no in-memory counter
in any worker.

## Concepts

| Object | Table | Meaning |
|---|---|---|
| Pool | `coding_slot_pool` (singleton `pool_id=1`) | `max_slots=3`; locked `FOR UPDATE` inside every grant tx — the cross-replica serializer |
| Queue entry | `coding_queue_entries` | A run competing for a slot. `fifo_seq` (identity) is the fixed FIFO tie-breaker |
| Grant | `coding_slot_grants` | One slot held by one run. `session_epoch` is the fencing token for stop confirmations |
| Audit | `coding_slot_events` | Append-only: `enqueued`, `granted`, `released`, `suspect`, `closed`, `stale_confirmation`, `duplicate_confirmation` |

Partial unique indexes enforce, at DB level: one non-released grant per
`slot_no`, per `run_id`, per `task_id`, per `entry_id`.

## Lifecycle

1. **Enqueue** — `SchedulerService.EnqueueForCoding(runID)`. Guard: run must
   be `preparing` **and** have an `allocated` worktree row — that pair is the
   "preparation done" evidence. Preparation, verify and QA never hold a slot;
   they never enter the queue either.
2. **Admit** — `SchedulerService.ScheduleOnce(ctx)`: lists waiting entries
   whose `recheck_at` has passed, fetches Jira priority + `is blocked by`
   links **outside any DB transaction**, sorts (priority rank desc →
   `fifo_seq` asc), then grants in order. Each grant is its own tx: pool lock
   → re-check entry waiting + run nonterminal (cancel race wins → entry
   `closed`, no slot) → existing active grant returned idempotently → lowest
   free `slot_no` → entry `granted` → audit.
3. **Heartbeat** — `RecordHeartbeat(grantID, epoch)` while the agent runs.
   Stale-epoch heartbeats are no-ops.
4. **Suspect** — `SweepSuspectGrants` flips `held`→`suspect` past the
   heartbeat TTL. The slot is **kept** and waits for an operator; heartbeat
   loss is never proof the agent stopped (AC-07). All three slots may sit
   suspect — that is the accepted safety trade-off.
5. **Release** — `ConfirmAgentStopped(grantID, epoch, reason)` is the *only*
   release path. A held/suspect grant releases exactly once; duplicate
   confirmations → `already_released`; a confirmation carrying an old
   `session_epoch` after re-grant → `stale_confirmation`, the new slot is
   untouched (AC-08).

## Blocked reasons (entry `block_reason`)

- `dependency_open:[KEYS]` — an `is blocked by` link resolved to a non-Done
  issue; re-evaluated after `recheck_at`.
- `jira_unverified` — Jira read failed or was insufficient to verify;
  no new grant until a later recheck succeeds.
- `run_terminal:<state>` — the run went terminal; entry `closed` permanently.

A blocked head never stalls eligible entries behind it.

## Priority & dependency

- Priority rank: Highest > High > Medium > Low > Lowest; unknown/absent → 0.
  Equal rank → smaller `fifo_seq` first. No starvation-relief mechanism yet.
- Dependencies verified at read time only. Jira and Postgres are not one
  atomic transaction — a status flip right after the check is an accepted
  staleness window.
- Module/repo collision checks are **operator-managed** — the scheduler
  deliberately does not infer scope or lock modules.

## VTX-7 handoff contract

VTX-7 (isolated coding agent runner) **must** check an active grant for its
run before starting any agent:

- `SELECT * FROM coding_slot_grants WHERE run_id=$1 AND status <> 'released'`
  — absent ⇒ the agent must not start.
- While running: `RecordHeartbeat(grant_id, session_epoch)` on the lease
  cadence (`heartbeat_at` drives the suspect sweep).
- On confirmed agent stop (exit, kill, supervisor confirmation):
  `ConfirmAgentStopped(grant_id, session_epoch, reason)` — idempotent, safe
  to retry, late/duplicate calls are no-ops.
- Cancel of a running task: stop the agent first, confirm, then release.
  A `cancelled` run state or an expired worktree lease is **not** evidence
  the process stopped.

## Metrics (AC-10)

`SchedulerService.Metrics()` + slog `metric` keys (`scheduler_pool_state`,
`scheduler_slot_granted`, `scheduler_slot_released`, `scheduler_grants_suspect`,
`scheduler_entry_blocked`, `scheduler_entry_closed`):

- `queue_waiting` / `queue_blocked` — queued vs parked-with-reason entries.
- `slots_held` / `slots_suspect` / `slots_free` / `slots_max` — heartbeat-held
  (suspect) is always distinguished from dependency-blocked.

SQL equivalent:

```sql
SELECT
  (SELECT COUNT(*) FROM coding_queue_entries WHERE status='waiting') AS queue_waiting,
  (SELECT COUNT(*) FROM coding_queue_entries WHERE status='waiting' AND block_reason IS NOT NULL) AS queue_blocked,
  (SELECT COUNT(*) FROM coding_slot_grants WHERE status='held') AS slots_held,
  (SELECT COUNT(*) FROM coding_slot_grants WHERE status='suspect') AS slots_suspect;
```

## Operator procedures

- **Migration**: `00006_coding_scheduler.sql` is additive (new tables only).
  Back up the database before applying per DoD.
- **Suspect slot resolution**: verify the agent process really stopped (worker
  logs / supervisor), then `ConfirmAgentStopped` with the grant's
  `session_epoch`. If the grant was already re-issued, the stale epoch is
  refused — re-read the grant first.
- **Re-queue a parked entry**: once its blocker resolves, it unblocks itself
  when `recheck_at` passes — no manual SQL needed. To force now:
  `UPDATE coding_queue_entries SET recheck_at = NULL WHERE entry_id = $1`.
- **Evidence status**: real three-agent concurrency evidence is **BLOCKED —
  waiting on VTX-7** (AC-11). Database-level tests prove the slot invariant;
  they are not agent evidence and must not be reported as such.
