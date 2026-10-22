# VTX-5 — AUTO-04 [Foundation] Scheduler & Resource Limits

Source: Jira VTX-5 (Ready To Develope, Medium). Spec text lives in the ticket description; this file is the implementation plan only.

## Objective

System-wide maximum of 3 concurrent coding-agent slots, persisted in PostgreSQL, with a durable FIFO+priority queue, atomic grant/release, Jira priority & "is blocked by" dependency gating, heartbeat-hold semantics, audit, and metrics. No agent execution (VTX-7 consumes the grant authority).

## Design (confirmed with ticket decisions)

- **Postgres is slot truth.** `coding_slot_grants` rows with partial unique indexes: one active grant per slot_no (1..3), per run, per task, per queue entry. A singleton `coding_slot_pool` row is locked FOR UPDATE inside the grant transaction → atomic across N schedulers, no in-memory counters.
- **Queue:** `coding_queue_entries` (run_id UNIQUE, `fifo_seq` IDENTITY as fixed FIFO tie-breaker, status waiting/granted/closed, `block_reason`, `recheck_at`). Entry allowed only for runs in `preparing` with an allocated worktree row (= "preparation done" evidence).
- **Ordering:** Jira priority fetched at decision time (rank Highest>High>Medium>Low>Lowest), tie-break `fifo_seq`. No DB tx held during Jira calls.
- **Dependencies:** Jira `issuelinks` type "Blocks" with `inwardIssue` → unresolved when linked issue statusCategory ≠ done → entry stays waiting with `block_reason=dependency_open:*` + `recheck_at`. Jira fetch failure → `block_reason=jira_unverified` + `recheck_at`. Blocked entries never stall eligible ones behind them.
- **Grant tx:** lock pool row → re-check entry still waiting + run still nonterminal (cancel race → close entry, no grant) → active grants < max → insert grant (session_epoch = per-run grant count+1, fencing token) → mark entry granted → audit row. Idempotent: existing active grant for the run is returned, not duplicated.
- **Release:** only `ConfirmAgentStopped(grant, epoch)` flips status→released. Duplicate confirm → already_released. Old-epoch confirm after re-grant → stale_confirmation, no release. Heartbeat loss → `MarkSuspectGrants` flips held→suspect (slot kept); no timeout auto-release.
- **Slot ≠ worktree lease.** Separate tables/mechanisms; neither auto-grants the other.
- **Audit:** `coding_slot_events` append-only (enqueued/granted/released/suspect/blocked/closed/stale_confirmation...).
- **Metrics:** `SchedulerMetrics()` read model (queue_waiting, queue_blocked, slots_held, slots_suspect, slots_free) + slog `metric` keys. Heartbeat-held (suspect) is reported separately from dependency-blocked.
- **Jira:** extend `integrations/jira` with `FetchSchedulingData(issueID)` → `{PriorityName, PriorityRank, UnresolvedBlockedBy []string}`. New port `SchedulingDataFetcher` in application layer.
- preparing→coding stays `stage_not_operational` (VTX-7 owns). No module/collision checks (operator-managed).

## Files

- `apps/backend/db/migrations/00006_coding_scheduler.sql` (additive; backup before apply per DoD)
- `apps/backend/db/queries/scheduler.sql` + `sqlc generate`
- `apps/backend/internal/application/scheduler_ports.go`, `scheduler_service.go`
- `apps/backend/internal/persistence/scheduler.go`
- `apps/backend/internal/integrations/jira/scheduling.go` (client extension) + unit test
- `apps/backend/internal/application/scheduler_integration_test.go` (`-tags integration`, real Postgres, multi-scheduler concurrency)
- `docs/runbooks/coding-scheduler.md` (ownership, VTX-7 handoff contract, reconcile, blocked-evidence note)

## AC mapping

- AC-01/02: grant cap enforced by pool row lock + slot unique index; concurrency test.
- AC-03: run/entry unique indexes + idempotent re-grant.
- AC-04: priority desc, fifo_seq asc; blocked entries skipped.
- AC-05: unresolved blocked-by or unverifiable → no grant, reason persisted.
- AC-06: grant tx re-checks run state; cancelled → entry closed, no slot.
- AC-07: suspect keeps slot; manual resolution path.
- AC-08: epoch-fenced confirm, idempotent, late/stale confirms safe.
- AC-09: enqueue requires `preparing` + allocated worktree; queued/blocked/other states rejected.
- AC-10: metrics + audit table.
- AC-11: real 3-agent evidence = BLOCKED pending VTX-7 (runbook notes this explicitly).

## Verify

`make test-boundaries; make vet; make typecheck; make test; make build` + `make test-integration` with TEST_DATABASE_URL against throwaway Postgres.
