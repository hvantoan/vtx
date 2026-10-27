# VTX-3 — TR-01..TR-25 Evidence Matrix

- **Spec:** SPEC-AUTO-02 v1.1 (Jira VTX-3)
- **Tree:** committed at `a0186816a87a7c07c2fda6f729ff858c77878690` when this matrix was captured
- **Local DB:** PostgreSQL 17 via Compose, schema migrated by `db/migrations` (goose up from empty and from the VTX-2 shape)
- **Rule:** a row is `PASS` only when a committed test or script reproduces it locally; everything unproven is `BLOCKED` with the reason.

## How to reproduce

```bash
# unit + API + domain (no DB)
go -C apps/backend test -race ./internal/...

# integration (real Postgres, real migrations, real HTTP router)
TEST_DATABASE_URL="postgres://vtx_user:vtx_password@localhost:49432/vtx_phase1_check?sslmode=disable" \
  make test-integration

# dispatcher / replicas / DLQ / crash / TR-07 (Compose stack)
./infra/scripts/test-registry-outbox.sh

# boundary gates (incl. TR-16 dependency rule)
make test-boundaries
```

## Matrix

| TR | What must hold | Evidence | Result |
|---|---|---|---|
| TR-01 | `execution_intent.created.v1` admits one task + one `queued` run with persisted identity | `internal/application/registry_consume_integration_test.go::TestConsumeIntentAdmitsThenReplays` (asserts `queued`, `version=0`, `attempt=0`, `commit_sha IS NULL`, run/task IDs present); `registry_service_test.go::TestParseIntentEventRequiresEveryContractField` | PASS |
| TR-02 | Replay of the same event 3× produces one run, one audit row, one outbound event | `TestConsumeIntentAdmitsThenReplays` (3 consumes, counts asserted) | PASS |
| TR-03 | Concurrent duplicate consumption on two dispatcher processes yields one run | `infra/scripts/test-registry-outbox.sh` TR-03 (2 replicas observed, 6 intents → 1 run each) | PASS |
| TR-04 | A legal advance writes version+1, exactly one audit row, exactly one outbound event | `registry_consume_integration_test.go::TestLeaseFencingAndTerminalRules` (begin: version 1, one `task_run_events` row, one `task_run.state_changed`); `TestTransitionRunOperationalPaths` | PASS |
| TR-05 | An invalid transition mutates nothing | `registry_service_test.go::TestTransitionRunWritesNothingWhenTheDomainRejects`, `TestTransitionRunRejectsUnknownActionBeforeTouchingStorage`; `TestTransitionRunRollsBackAtomically` | PASS |
| TR-06 | A terminal run cannot restart without a new intent | `TestLeaseFencingAndTerminalRules` (cancel → terminal; second begin → `ErrAlreadyTerminal`, row unchanged) | PASS |
| TR-07 | 20 simultaneous updates across ≥2 API instances commit exactly once | `test-registry-outbox.sh` TR-07: 2 API replicas driven directly (`docker exec … wget`), 20 writers on `expected_version=0` → 1×200 + 19×409, `version=1`, one audit row | PASS |
| TR-08 | Crash/restart leaves consistent state and no duplicated effects | `test-registry-outbox.sh` TR-08/TR-20 (3× `SIGKILL` cycles: no duplicate run/audit/outbound) | PASS |
| TR-09 | Stale/expired lease or epoch is rejected without side effects | `domain/registry_test.go::TestLeaseBranches`; `TestLeaseFencingAndTerminalRules` (SQL-injected lease: stale epoch and expired lease rejected on a real row, version and state unchanged) | PASS |
| TR-10 | Multiple re-entries of one issue are deferred, FIFO, and never lost | `TestConsumeIntentDefersWhileARunIsActive`; `TestConcurrentDistinctIntentsForOneTask` (1 admitted + 1 `pending_deferred`); harness TR-24 drains 3 deferred intents | PASS |
| TR-11 | Migrations apply up/down/up and `sqlc generate` is reproducible | `internal/persistence/db/registry_integration_test.go::TestMigrationLifecycle` (goose up/down/up); the Compose harness applies the same migrations to an existing database (`up` on `vtx_platform`); CI step `sqlc generate drift check`, re-run locally with no diff | PASS |
| TR-12 | Retry with backoff, poison payloads quarantined with a reason | `application/outbox_dispatcher_test.go::TestDispatchOnceQuarantinesPoisonAndExhaustedRetries`; `test-registry-outbox.sh` TR-12 (payload without `jira_site_id` → `dead_lettered_at` + reason) | PASS |
| TR-13 | Unsupported event types are never claimed and cannot corrupt registry state | `TestDispatchOnceClaimsOnlyItsOwnEventTypes`; `test-registry-outbox.sh` TR-13 (`task_run.created.v1` never claimed/published/dead-lettered) | PASS |
| TR-14 | Authenticated mutation, paginated audit read, safe logs | `internal/transport/http/handlers_registry_integration_test.go::TestRegistryAPIAgainstPostgres` (401 anonymous, 403 wrong actor, 200 mutation, version-ordered audit, cursor page, task read); `handlers_registry_test.go::TestRegistryAuthRejectsMissingAndUnknownTokens`; `test-registry-outbox.sh` SECRETS (no token in logs) | PASS |
| TR-15 | `blocked → resume`, `retry_wait → retry_due` (resume_state), explicit cancel | `TestTransitionRunOperationalPaths` (block/resume + `schedule_retry` parks with `attempt=1`; `retry_due` before due time rejected, after due time restores `queued` and clears bookkeeping); `TestLeaseFencingAndTerminalRules` (cancel → `cancelled`, `finished_at`) | PASS |
| TR-16 | Registry packages do not depend on Temporal, Git, agents or Docker | `make test-boundaries` → Check 7 (`go list -deps ./internal/domain ./internal/application ./internal/transport/http ./internal/persistence` free of `go.temporal.io`, `go-git`, `docker`, `internal/orchestration`) | PASS |
| TR-17 | A failure mid-transaction rolls the whole transition back | `TestTransitionRunRollsBackAtomically` (injected failure: no state change, no audit, no outbox) | PASS |
| TR-18 | Upgrade on a populated VTX-2 database without destructive migration | `TestMigrationLifecycle` (goose up/down/up on a populated DB); `tests/evidence/vtx-3-migration-precheck.log`; `tests/evidence/vtx-3-compatibility-matrix.md`; `00004_task_registry.sql` is additive only (no `DROP`) | PASS |
| TR-19 | Two distinct intents for one task: one admitted, the other deferred in FIFO order | `TestConcurrentDistinctIntentsForOneTask` (two pools, two goroutines, same issue: 1 admitted, 1 deferred, 1 active run, 1 `pending_deferred` row) | PASS |
| TR-20 | Crash after commit, before ACK → replay produces no duplicate effect | `TestDispatchReplayAfterEffectCommittedBeforeAck` (deterministic: effect committed with the source event unpublished → replay ACKs, creates no second run/audit/outbound); `outbox_dispatcher_test.go::TestDispatchOnceAcksOnlyAfterTheEffect` (effect before ACK); `test-registry-outbox.sh` TR-08 (kill cycles) | PASS |
| TR-21 | Lease handover: stale epoch or expired lease rejected even when the version matches | `TestLeaseFencingAndTerminalRules` (version matches, epoch stale → reject; epoch matches, lease expired → reject; live matching lease → accept) | PASS |
| TR-22 | Unsupported/invalid payload → DLQ; retryable fault → retry; redrive cannot loop forever | Harness TR-12 and TR-22 (redriven poison event dead-lettered again, no infinite loop); unit retry/exhaustion test | PASS |
| TR-23 | Actor cannot be spoofed from the body; audit pagination is stable on equal timestamps | `handlers_registry_test.go::TestRegistryBodyActorCannotImpersonate`, `TestRegistryActorComesFromTheToken`, `TestRegistryEventCursorUsesVersion`; `TestRegistryAPIAgainstPostgres` (cursor = version, never `created_at`) | PASS |
| TR-24 | Populated VTX-2 DB migrates in place with pending outbox preserved and IDs intact | `TestMigrationLifecycle`; `test-registry-outbox.sh` TR-24 (3 pending intents seeded before the worker starts → drained to 0 with no extra job); `vtx-3-migration-precheck.log` (row counts before/after) | PASS |
| TR-25 | Failed CAS, invalid transition and injected transaction failure all leave consistent state | `TestTransitionRunConcurrentWriters` (20 writers, one commit), `TestTransitionRunRollsBackAtomically`, `TestLeaseFencingAndTerminalRules` (rejected transition writes nothing) | PASS |

## DoD items that stay BLOCKED

| Item | Status | Why | Unblock |
|---|---|---|---|
| CI green at an exact SHA | **BLOCKED** | No CI verdict is claimed for this matrix. Local `make vet`, `make typecheck`, `make test`, `make test-boundaries`, `make test-integration`, `make build` and the Compose harness pass, but that is not a CI verdict. | Push the commit and attach the green run for that SHA. |
| 20-writer concurrency in CI | PASS locally, **BLOCKED in CI** | TR-07 drives two API replicas through `docker exec`; the workflow runs the registry harness, but the CI run for this SHA does not exist yet. | The workflow step exists (`VTX-3 registry outbox harness`); it turns into evidence on the first green run. |

## Reviewer notes

- `published_at` on the source `execution_intent.created.v1` row means "this subscriber is done with it". It is **not** a global delivery proof for `task_run.*` events, which stay pending until a downstream consumer exists (documented in `application/outbox_events.go` / dispatcher tests).
- `retry_due_at` is enforced by the domain (`ActionRetryDue` before the due time is an `invalid_transition`); the attempt **budget** is VTX-5's, not this state machine's.
- Lease rule is literal §0.D: epoch/owner/expiry are compared *if an active lease exists*. No active lease → policy/operator actions are allowed from any nonterminal state (`queued`/`blocked` are the usual pre-lease states, not an allow-list). A present-but-expired/foreign lease is `stale_lease` from every state. TR-15 still injects a live lease for `retry_due` to exercise the matching-epoch path (and `begin` requires one by definition).
- Retry backoff is `min(base·2^attempt, cap 5m) + jitter`, grouped per attempt inside one batch (`TestRetryDelayGrowsExponentially`). A fixed 5s delay would have burned `MaxAttempts` in ~25s.
- `task_run.intent_deferred.v1` carries `task_id` (VTX-5 resolves the queue by task); the defer branch has no run, so the store passes the task identity into the outbound closure.
- `GET /tasks/{id}/runs` was removed: it is not in §7's read list, and a `version` cursor over mutable run versions skips/overlaps pages.
- Stage transitions (`preparing…pr_draft`, `pr_draft → completed`) are intentionally contract-only in phase 1: the table exists and the actor check passes, but they return `stage_not_operational` (HTTP 409) and are covered by `domain/registry_test.go::TestNonOperationalHopsAreClosed`.
