# Runbook: Coding Slot Scheduler (VTX-5 / AUTO-04)

## What it owns

The scheduler admits tasks to coding execution under a **system-wide budget
of 3 coding slots** — shared across every project and every worker, not
three per worker. Only the coding agent occupies a slot; worktree
preparation, verification and QA never hold one.

Two tables are the source of truth (PostgreSQL, same DB as the registry):

| Table | Purpose |
|---|---|
| `coding_queue` | Durable admission queue. One row per run (`run_id` UNIQUE). Statuses: `waiting`, `allocated`, `blocked_dependency`, `blocked_unverified`, `cancelled`, `released`. |
| `coding_slots` | Held/released grants. `grant_id` is the fencing token VTX-7 presents before starting an agent. |
| `scheduler_events` | Scheduler audit trail (enqueued / granted / released / cancelled / blocked). Distinct from `task_run_events`: these are not run-state transitions. |

## Admission contract

1. **Enqueue** — `SchedulerService.Enqueue(run_id)`. Requires an existing,
   nonterminal run. Idempotent on `run_id`. Records the Jira priority at
   enqueue; admission re-verifies before every grant.
2. **Admit** — `SchedulerService.Admit(worker)`. Evaluates every
   `waiting`/`blocked_*` entry: fetches Jira priority + inward "is blocked
   by" links **outside any DB transaction**, then runs a single
   advisory-locked transaction (`pg_advisory_xact_lock(420005)`) that grants
   slots in `(priority_rank, fifo_seq)` order until the budget is full.
3. **Priority** — `Highest > High > Medium > Low > Lowest > unknown`.
   FIFO (`fifo_seq`, durable) breaks ties inside one priority. A blocked or
   unverifiable head never starves eligible entries behind it.
4. **Dependency gating** — any "is blocked by" issue not in status category
   `done`, or any Jira read failure, means **no slot**. The entry is parked
   as `blocked_dependency` / `blocked_unverified` with a stored reason and
   re-evaluated on the next pass.
5. **Cancel** — `CancelQueued(run_id)` atomically cancels
   `waiting`/`blocked_*` entries only. An `allocated` entry returns
   `ErrSlotHeld`: stopping a running task is agent-stop → `ConfirmAgentStopped`.

## Heartbeat and release (AC-07/08)

- `RecordHeartbeat(run_id, grant_id)` refreshes the heartbeat; only the
  recorded grant may refresh its own slot.
- `SweepStaleHeartbeats(maxAge)` flags `suspected` on quiet slots. It
  **never releases**: a slot is held until a human confirms the agent has
  stopped. Three dead workers can park all three slots — that is the
  accepted safety trade-off. Look for `metric=scheduler_slot_suspected`
  warnings; resolve by confirming the agent stopped, then call
  `ConfirmAgentStopped`.
- `ConfirmAgentStopped(run_id, grant_id, reason)` releases **exactly once**.
  A stale, duplicate or late confirmation releases nothing and writes an
  `ignored_stale_confirmation` audit row. A stopped-session grant can never
  free a newer grant on the same run.

## Metrics (AC-10)

`GET /api/v1/scheduler/metrics` (registry actor token) returns
`queue_waiting`, `queue_allocated`, `blocked_dependency`,
`blocked_unverified`, `slots_held`, `slots_suspected`, `slots_max`.

## Boundaries with other tickets

- VTX-3 registry owns run state and the run lease. The scheduler never
  mutates `task_runs` and never uses nonterminal-run counts as a
  coding-active proxy (AC-09).
- VTX-4 owns worktree allocation and its lease. Slot grants are independent:
  renewing or expiring a worktree lease neither grants nor frees a slot.
- VTX-7 executes the agent and must present a held `grant_id` before
  starting. The `preparing → coding` guard in the state machine is NOT
  opened by this ticket — no fake evidence.
- AC-11 evidence (three real coding agents running concurrently) is
  **BLOCKED pending VTX-7** and is tracked separately from scheduler
  acceptance.

## Failure modes

| Symptom | Meaning | Action |
|---|---|---|
| `blocked_dependency` growing | Jira "is blocked by" links unresolved | Check the linked issues; entries re-evaluate on next `Admit`. |
| `blocked_unverified` growing | Jira unreachable / permission loss | Restore Jira access; entries self-heal on next pass. |
| `slots_suspected` > 0 | Worker lost heartbeat, slot retained | Confirm the agent stopped, then `ConfirmAgentStopped`; never force-delete rows. |
| Queue grows, `slots_held` = 3 | Budget full, correct behavior | Wait for releases; do not add a second counter anywhere. |
