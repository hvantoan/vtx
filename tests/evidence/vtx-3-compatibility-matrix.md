# VTX-3 — Migration compatibility matrix (§0.A)

Verified against `85a7d9a` by reading `db/migrations/00001..00003`, `db/queries/{jira,tasks}.sql`, `internal/application/outbox_events.go`, `contracts/events/v1/events.json`, and by running the precheck queries against a populated Postgres. Machine-readable precheck output: `tests/evidence/vtx-3-migration-precheck.log`.

## Precheck (branch condition of gate G1)

| Reading | Value at capture | Meaning |
|---|---|---|
| goose version | `3` | `00003` is the head before `00004` |
| `tasks` / `task_events` / `agent_sessions` | `0` / `0` / `0` | bootstrap tables are dead but referenced by FKs — renaming them costs nothing and buys nothing, so they are left alone |
| `execution_intents` | `1` | VTX-2 data exists; it must survive the migration |
| `outbox_events` | `1` (+ `1` pending) | one un-published VTX-2 event ⇒ the dispatcher introduced by VTX-3 has a real backfill case |
| FK into bootstrap tables | `task_events → tasks`, `agent_sessions → tasks` | nothing else depends on `tasks` |
| `pg_extension` | `plpgsql` only | PG17 provides `gen_random_uuid()` in core; no `pgcrypto`, no new extension |

## Object-by-object disposition

| Existing object | Source | How VTX-3 uses it | Action |
|---|---|---|---|
| `tasks(id VARCHAR(64) PK, title, status, prompt, workspace_path, created_at, updated_at)` | `00001` | Name/schema clash with the spec's `tasks` | **PRESERVE** — VTX-3 uses the new `registry_tasks`; no copy, no rename, no `db/queries/tasks.sql` rewrite |
| `task_events(id BIGSERIAL, task_id → tasks, seq, event_type, payload, created_at)` | `00001` | Bootstrap audit table | **PRESERVE** |
| `agent_sessions(task_id → tasks, …)` | `00001` | Bootstrap table with FK | **PRESERVE** |
| `execution_intents(intent_id UUID PK, source, issue_key, target_status_id, status, …)` | `00002` | Source of `task_runs.intent_id UNIQUE`; identity (`jira_site_id`/`issue_id`/`changelog_id`) is read from the outbox payload, **not** added as columns | **UNCHANGED** |
| `jira_webhook_inbox(…)` + `UNIQUE(jira_site_id, issue_id, changelog_id, target_status_id)` | `00002` | Authoritative cross-reference for identity fields if the payload ever needs verification | **UNCHANGED** |
| `outbox_events(outbox_id, intent_id NOT NULL → execution_intents ON DELETE CASCADE, event_type, payload, published_at, created_at)` + `(created_at) WHERE published_at IS NULL` | `00002` | Claim source for the dispatcher and the sink for `task_run.*` events | **ALTER ADD** `claimed_by`, `claimed_until`, `attempt`, `next_attempt_at`, `dead_lettered_at`, `dead_letter_reason`; every VTX-2 column and index keeps its meaning |
| `webhook_receipts(…)` | `00003` | Unrelated (per-request webhook audit) | **UNCHANGED** |
| `execution_intent.created.v1` payload (`event_id, intent_id, issue_id, issue_key, jira_site_id, changelog_id, target_status_id, occurred_at, occurred_at_source, correlation_id`) | `internal/application/outbox_events.go` | Already carries every field §2 requires; a missing field is a schema error, never a synthesised default | **UNCHANGED** |
| `contracts/events/v1/events.json` (`execution_intent.created.v1` only) | `contracts` | Consumer filters by `event_type` | **ADD** `task_run.created.v1`, `task_run.state_changed.v1`, `task_run.intent_deferred.v1` (Phase 6) |
| Outbox dispatcher | — | Does not exist: `InsertOutboxEvent` writes rows, nothing ever set `published_at` | **NEW** (Phase 4) — one designated consumer identity |
| Dispatcher process | `infra/compose/*` | No such service (postgres/temporal/temporal-ui/backend/gateway/web only) | **ADD** service `registry-worker` running `/app/worker registry` (Phase 4) |
| Migration sequence | `00001..00003` | — | **`00004_task_registry.sql`** |

## Causal note for reviewers: `outbox_events.intent_id NOT NULL`

Every event the registry publishes (`task_run.*`) must carry `intent_id`, and the FK is `ON DELETE CASCADE`. Both facts hold because `task_runs.intent_id` is `UNIQUE NOT NULL` and every run has exactly one intent, so the value is always available. The consequence to weigh later: a future retention policy that deletes `execution_intents` would silently delete still-pending outbox rows. The repo has no delete path today, so the CASCADE is inert — recorded here so the trade-off is a decision, not an accident.

## Evidence for the four P1 gates

| Gate | Requirement | Evidence produced by VTX-3 |
|---|---|---|
| G1 no destructive migration | Preserve bootstrap + VTX-2 data; tested rollback | `TestMigrationLifecycle`: `down-to 0` → `up-to 3` → seed VTX-2 intent + pending outbox → `up` → counts and ids unchanged → `down-to 0` → `up`. Runs in `make test-integration` and in the `backend-integration` job |
| G2 consumer-specific delivery | Exactly one persisted effect on replay; source ACK ≠ outbound delivery | `inbox_consumptions` PK `(consumer_name, source_event_id)`; `MarkOutboxPublished` idempotent on `published_at IS NULL`; claim/retry/quarantine statements in `db/queries/outbox.sql` |
| G3 runtime isolation | Registry processes work without Temporal | `registry-worker` compose service has no `temporal` dependency and `cmd/worker registry` never dials Temporal |
| G4 evidence-gated stages | Actor alone cannot advance into completion/PR | Domain guard set (Phase 2) + negative tests per hop (Phase 3/5) |
