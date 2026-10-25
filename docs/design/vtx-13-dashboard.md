# VTX-13 — Operator Dashboard — Design Specification

**Epic:** VTX-13 (Dashboard / pilot)
**Status:** Design spec — implementation-grade, English
**SRS coverage:** §4.6 FR-UI-001..004, §2.1 Operator/Tech Lead, §3 UC-06/UC-07, §5 state model (operator-driven `block`/`resume`/`cancel`), §6 data contracts, §7 Registry API + Web interfaces, §8 NFR-OPS-001 / NFR-SEC-001 / NFR-AUD-001 / NFR-PRIV-001 / NFR-CON-001, §9 verification (VTX-13 band, E2E-03/06/07), §11 OD-03/OD-04/OD-06
**Grounding:** `apps/web/src/app/page.tsx`, `apps/web/src/lib/api.ts`, `apps/web/src/types/api.ts`, `contracts/openapi/v1/openapi.yaml`, `apps/backend/internal/transport/http/{routes.go,handlers.go,handlers_registry.go,middleware.go}`, `internal/application/{registry_ports.go,outbox_dispatcher.go,registry_service.go}`, `internal/domain/registry.go`, `internal/config/{config.go,registry.go}`, `docs/design/design-guidelines.md`, `docs/wireframe/dashboard.html`, migrations 00001–00005.

---

## 1. Overview

VTX-13 turns the Phase-0 cockpit (`/`, platform health only) into an **operator console** for the pipeline: measured subsystem readiness, live run inventory with stage progress, a per-run drill-down (timeline, audit, evidence, commit SHA, PR link, error disposition), and **authenticated, audited operator mutations** (block / resume / cancel / DLQ redrive / PR sync). Realtime updates flow over the existing SSE stream, hardened and extended — auth always via `Authorization` header, never a query-string token.

Three honesty rules carried over from `design-guidelines.md` and the SRS:

1. **Measured, not hardcoded** (FR-UI-001): every status tile traces to a real probe/table — `/health`, `/ready` (extended), `worker_heartbeats`, `outbox_events` aggregates, `task_runs`. Unknown/unpolled renders as `UNKNOWN`, never a fabricated value.
2. **Authenticated mutations with audit** (FR-UI-003): the only run-mutation path stays `POST /api/v1/registry/runs/{run_id}/transitions` under the `operator` actor token; the audit row records actor + sanitized reason + correlation; a new `operator_audit_log` table covers mutations that have no `task_run_events` row (DLQ redrive, PR sync).
3. **Credential in header, always** (FR-UI-004): SSE keeps the `fetch` + `Authorization: Bearer` pattern already implemented in `page.tsx` (EventSource cannot set headers — this is why `fetch` streaming is used). A `?token=` query parameter is explicitly **not** a credential and must never be accepted.

Scope boundary: the dashboard **reads** registry/worktree/PR data through typed views and **mutates** only through the existing transition endpoint and two new operator actions (DLQ redrive, PR sync). It never bypasses the state machine — there is no "set state" endpoint.

---

## 2. SRS Coverage Table

| SRS ID | Requirement (paraphrased) | Design decision |
|---|---|---|
| FR-UI-001 | Issue/run state, stage progress, worker health, dependency readiness — measured, not hardcoded | Read endpoints §4.3: `GET /operator/overview` (state histogram, deferred count, admission slots 0–3 from `task_runs`), `GET /operator/workers` (`worker_heartbeats` staleness = 3×beat), `/ready` extended with a real `outbox` dependency probe (lag measured via `OutboxLag`). Stage progress derived from `state` position in the §5 chain (§5.3). |
| FR-UI-002 | Run timeline, event/audit history, commit SHA, CI/QA evidence, PR link, error disposition | `GET /operator/runs/{run_id}` aggregates: run (sha/branch/error fields), `task_run_events` timeline (existing registry events endpoint), worktree, `pull_requests` + `ci_status`, `gate_verdicts.evidence`, last `error_code`/`error_message`. UI `RunDetailPage` §6.2. |
| FR-UI-003 | Operator mutations need authenticated principal + authorization; audit actor/reason/correlation | Mutations = existing `transitions` endpoint (operator actor token, `actor_type=human` audit) + new `POST /operator/outbox/dlq/{id}/redrive` and `POST /pull_requests/{run_id}/sync`, all under `RegistryActorMiddleware` restricted to `operator` (+`policy` for sync). `X-Operator-Id` header carried into audit metadata; `operator_audit_log` table §3. |
| FR-UI-004 | SSE/stream via Authorization header or session; never token in query string | Existing `GET /api/v1/events` keeps Bearer auth (§4.5). Server-side guard: request carrying `token`/`access_token`/`auth` query params is rejected 401 `credential_in_query`. `run_id` filter and `Last-Event-ID` are the only new query/header inputs — neither is a credential. |
| §7 Registry API | Read task/run/history + authorized mutation, typed errors 401/403/404/409 | Dashboard consumes the existing `/api/v1/registry/*` surface for timeline/mutations; new `/api/v1/operator/*` surface reuses the same middleware and error envelope (`ErrorResponse`), same status semantics (§4.6). |
| §7 Web | REST + authenticated SSE / fetch stream; no misleading health claims | Fetch-streaming SSE (not EventSource) is the contract; readiness tiles display the actual dependency statuses including `degraded`; stale data (>3×poll) is visually dimmed per design-guidelines §4.1 `stale` state. |
| UC-06/UC-07 | Operator block/resume/cancel; view state/timeline/evidence/DLQ/outbox lag; redrive with audit | `OperatorActions` UI component → transitions endpoint; `OutboxPanel` → `GET /operator/outbox` + DLQ list + redrive (audited in `operator_audit_log`). |
| NFR-OPS-001 | Dependency health real; outbox lag, retries, DLQ, active runs observable | `/ready` gains `dependencies.outbox` (`up`/`degraded`/`down` by lag threshold `READY_OUTBOX_LAG_WARN`, default 5m, env-configurable). `GET /operator/outbox` returns per-event-type pending/retrying/dead-lettered counts + oldest age. |
| NFR-SEC-001 | Fail-closed APIs, secret redaction | No new unauthenticated route; operator surface is a separate middleware group from the dashboard token (mirroring the worktree group in `routes.go`); `operator_audit_log` stores actor names/ids, never tokens. |
| NFR-PRIV-001 | Private-network dashboard option; secrets out of source | Operator token supplied via env (`REGISTRY_ACTOR_OPERATOR_TOKEN`, existing); the web app calls the API with a configured bearer — MVP pattern documented §8 D3; no secrets committed (Next.js only receives non-secret config values). |
| NFR-AUD-001 | Traceable issue→intent→run→SHA→PR; no token/raw payload in logs | Run detail view joins exactly that chain: `registry_tasks` → `execution_intents`/`task_runs` → `commit_sha`/`branch` → `pull_requests`. `X-Operator-Id` is a display identifier, not a credential. |
| NFR-CON-001 | 20+ concurrent writers, exactly one commits | Dashboard mutations go through `TransitionRun` (existing CAS); the UI always sends `expected_version` from the freshest `GET` and surfaces `stale_version` 409s as "refresh and retry" — it never retries mutations automatically (§6.4). |
| E2E-03/06/07 | Crash recovery visibility; admission ≤3 visible; operator actions + DLQ redrive per policy + audit | Tests UI-10/11/13 (§7): heartbeats mark crashed worker `down`; overview counts `queued`/`deferred` against the 3-slot admission invariant read from live `task_runs`; redrive writes `operator_audit_log` + clears `dead_lettered_at`. |
| OD-03 | Multi-consumer ACK semantics undecided | Dashboard SSE does **not** consume/ACK outbox events — read-only tailer §4.5 D1 sidesteps OD-03 entirely. |
| OD-06 | SLO/quota params TBD | Poll cadences + heartbeat intervals are config (§4.7), not hardcoded; thresholds flagged where numbers are placeholders pending stakeholder baseline. |

---

## 3. Data Model / DDL Deltas (future migration `00007_operator_dashboard.sql`)

Additive only. Lands after `00006_draft_pr.sql` (vtx-12 spec); the `outbox_events.redrive_count` column is already declared there with `IF NOT EXISTS` — whichever migration merges first owns it.

```sql
-- Worker liveness, MEASURED not polled-from-nowhere (FR-UI-001). Each loop
-- (outbox dispatcher, temporal worker, scheduler, future agent-runtime
-- bridge) upserts its row on a beat interval; staleness is computed at read
-- time (last_beat_at older than 3× configured interval ⇒ 'down').
CREATE TABLE IF NOT EXISTS worker_heartbeats (
    worker_id TEXT NOT NULL,
    worker_kind TEXT NOT NULL CHECK (worker_kind IN
        ('temporal', 'dispatcher', 'scheduler', 'agent_runtime')),
    queue TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    last_beat_at TIMESTAMPTZ NOT NULL,
    active_runs INT NOT NULL DEFAULT 0,
    metadata JSONB,                        -- version, host, config digest — display only
    PRIMARY KEY (worker_id, worker_kind)
);
CREATE INDEX IF NOT EXISTS idx_worker_heartbeats_beat
    ON worker_heartbeats (last_beat_at);

-- Audit for operator mutations that produce no task_run_events row
-- (DLQ redrive, PR sync, future operator settings). Run-state mutations
-- keep their existing audit in task_run_events — this table complements,
-- never replaces it.
CREATE TABLE IF NOT EXISTS operator_audit_log (
    audit_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor TEXT NOT NULL,                   -- authenticated actor ('operator')
    operator_id TEXT,                      -- X-Operator-Id subprincipal, nullable
    action TEXT NOT NULL,                  -- 'dlq.redrive' | 'pull_request.sync' | ...
    target_type TEXT NOT NULL,             -- 'outbox_event' | 'pull_request' | 'task_run'
    target_id TEXT NOT NULL,
    reason TEXT,
    correlation_id VARCHAR(128),
    request_id VARCHAR(128),               -- chimw.RequestID
    outcome TEXT NOT NULL CHECK (outcome IN ('applied', 'rejected', 'error')),
    detail JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_operator_audit_target
    ON operator_audit_log (target_type, target_id, created_at DESC);

-- Redrive bookkeeping (also declared in 00006 — keep IF NOT EXISTS here).
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS redrive_count INT NOT NULL DEFAULT 0;

-- Dashboard list queries: state histogram + recency scans hit these.
CREATE INDEX IF NOT EXISTS idx_task_runs_state_updated
    ON task_runs (state, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_outbox_dead_lettered
    ON outbox_events (dead_lettered_at) WHERE dead_lettered_at IS NOT NULL;
```

New sqlc query file `db/queries/operator.sql`: `RunStateHistogram`, `CountDeferredIntents`, `ListRunsFiltered` (state/issue_key cursor paginated on `(updated_at, run_id)`), `OutboxStatsByType` (pending/retrying/dead-lettered + `MIN(created_at)` lag), `ListDeadLettered`, `RedriveDeadLetter` (clears `dead_lettered_at`, resets `next_attempt_at`, increments `redrive_count` in one UPDATE), `UpsertWorkerHeartbeat`, `ListWorkerHeartbeats`, `InsertOperatorAudit`.

---

## 4. Go Package / API Design

### 4.1 Layout

```text
internal/application/
    operator_ports.go      // OperatorReader, OperatorMutator, HeartbeatWriter ports
    operator_service.go    // OperatorService: read aggregation + redrive/sync mutations
    operator_service_test.go
internal/persistence/
    operator.go            // pgx impl (queries from db/queries/operator.sql)
internal/transport/http/
    handlers_operator.go   // /api/v1/operator/* handlers
    sse.go                 // extracted from handlers.go + run_id filter + credential-in-query guard
    sse_tailer.go          // read-only outbox tailer → BroadcastEvent (§4.5)
internal/config/
    operator.go            // ValidateOperator() (thresholds, heartbeat intervals)
cmd/api/main.go            // wires heartbeat writers + tailer alongside dispatcher
```

### 4.2 Ports

```go
type OperatorReader interface {
    RunStateHistogram(ctx) (map[domain.State]int64, error)
    ListRuns(ctx, f RunFilter, cursor *RunCursor, limit int32) ([]OperatorRunView, error)
    GetRunDetail(ctx, runID string) (RunDetailView, error) // run+task+worktree+pr+gate verdict
    OutboxStats(ctx) ([]OutboxTypeStat, error)
    ListDeadLettered(ctx, cursor *int64, limit int32) ([]DlqItem, error)
    ListWorkerHeartbeats(ctx) ([]WorkerHeartbeatView, error)
}

type OperatorMutator interface {
    // RedriveDeadLetter clears dead-letter state + writes the audit row in
    // ONE transaction. Idempotent on already-redriven ids via the UPDATE's
    // WHERE dead_lettered_at IS NOT NULL → 0 rows ⇒ ErrNotDeadLettered.
    RedriveDeadLetter(ctx, outboxID string, audit OperatorAuditRecord) error
    InsertOperatorAudit(ctx, rec OperatorAuditRecord) error
}

type HeartbeatWriter interface { Beat(ctx, hb WorkerHeartbeat) error }
```

`RunDetailView` composes existing `RunView`/`TaskView`/`EventView` plus `WorktreeView`, `PullRequestView` (from vtx-12), and `GateVerdictView{verdict, policy_version, evidence, decided_at}` — one JSON document per run so the detail page makes exactly one request.

### 4.3 HTTP surface — `RegistryActorMiddleware` group, actor-restricted

| Route | Actors | Purpose |
|---|---|---|
| `GET /api/v1/operator/overview` | operator, policy | `{runs_by_state, deferred_count, admission:{max:3, active}, outbox:{pending,retrying,dead_lettered,oldest_age_seconds}, workers_up/down}` |
| `GET /api/v1/operator/runs?state=&issue_key=&cursor=&limit=` | operator, policy | Paginated run list joined to task (`issue_key`), pr_url, commit_sha — the `/runs` table |
| `GET /api/v1/operator/runs/{run_id}` | operator, policy | `RunDetailView` aggregate (§4.2) |
| `GET /api/v1/operator/outbox` | operator, policy | Per-event-type `{pending,retrying,dead_lettered,oldest_age_seconds}` |
| `GET /api/v1/operator/outbox/dlq?after=&limit=` | operator, policy | Dead-lettered events (id, type, reason, attempts, redrive_count, dead_lettered_at) |
| `POST /api/v1/operator/outbox/dlq/{outbox_id}/redrive` | **operator only** | Body `{reason}`; writes `operator_audit_log` + clears DL in one tx; 409 `not_dead_lettered` |
| `GET /api/v1/operator/workers` | operator, policy | Heartbeat rows + computed `status` (`up`/`down`) + Temporal worker list merged |

Policy-actor read access keeps automation (retry orchestrator) able to observe without a second credential; **mutations are operator-only** — a scheduler/orchestrator token gets 403 `forbidden`, matching `domain.Evaluate`'s actor gating style.

### 4.4 `X-Operator-Id` header

Optional, ≤64 chars, sanitized like `reason` (whitespace-flattened). Middleware copies it to context; transition handler injects `metadata.operator_id` so `task_run_events.metadata` distinguishes which human used the shared operator token. For `operator_audit_log` it lands in `operator_id`. **Not a credential** — the bearer token still authenticates; this is display-level attribution pending a real IdP (§8 D4).

### 4.5 SSE hardening + feed (FR-UI-004, OD-03)

- **Auth:** unchanged `BearerAuthMiddleware` on `GET /api/v1/events`. New middleware `RejectQueryCredentials`: presence of `token`, `access_token`, `auth`, or `api_key` query params ⇒ 401 `credential_in_query` — makes the "never in query string" rule a server-enforced contract, not a client convention.
- **Feed source (DECIDED, avoids OD-03):** `sse_tailer.go` runs a read-only poll `SELECT outbox_id, event_type, payload, created_at FROM outbox_events WHERE event_type IN ('task_run.created.v1','task_run.state_changed.v1','task_run.intent_deferred.v1','pull_request.opened.v1') AND created_at > $cursor ORDER BY created_at` every `SSE_TAIL_INTERVAL` (default 2s). It **never claims or ACKs** — the outbox stays exclusively owned by its real consumers, so no event is stolen from VTX-5/VTX-12 consumers.
- **Dedup/resume:** SSE `id:` field = `outbox_id`; browser `Last-Event-ID` (or `?last_event_id=`) resumes the cursor; reconnect replays from cursor, never fabricates. In-memory hub keeps no history beyond the current poll window — the DB is the buffer.
- **Filter:** optional `?run_id=<uuid>` narrows the tail to one run (payload `run_id` match server-side).
- Heartbeat `system.heartbeat` frames every 15s keep proxies from closing the stream (existing behavior retained).

### 4.6 Error semantics for operator endpoints

Same envelope `{"error","message"}` + same codes family as registry: `unauthorized` 401, `forbidden` 403, `not_found` 404, `invalid_request` 400, plus `not_dead_lettered` 409, `credential_in_query` 401, `sync_throttled` 409 (PR sync ≤1/60s per run). `RegistryActorFromContext` supplies the actor; handlers check the allowlist per route — a token that authenticates but isn't permitted is always 403, never silent.

### 4.7 Config additions (`ValidateOperator()`)

`SSE_TAIL_INTERVAL` (2s), `SSE_TAIL_BATCH` (200), `WORKER_HEARTBEAT_INTERVAL` (10s), `WORKER_STALE_FACTOR` (3 ⇒ down after 30s), `READY_OUTBOX_LAG_WARN` (5m ⇒ `degraded`), `OPERATOR_PAGE_MAX` (200, matching registry `maxPageLimit`), `DASHBOARD_API_TOKEN` — see §8 D3. All are env-backed with dev defaults; production values pending OD-06 baseline.

---

## 5. State Machine Interactions (SRS §5)

### 5.1 The dashboard drives exactly four contract actions

| UI action | Transition call | Actor | From states | To |
|---|---|---|---|---|
| Block | `block` + reason | operator | any nonterminal | `blocked` (resume_state parked) |
| Resume | `resume` | operator | `blocked` | stored `resume_state` |
| Cancel | `cancel` + reason | operator | any nonterminal | `cancelled` (terminal) |
| Redrive DLQ | *not a run transition* — outbox mutation + audit | operator | — | event re-queued for its consumer |

`schedule_retry`/`retry_due`/`fail`/`advance`/`begin`/`complete` are **not** exposed in the operator UI — they belong to scheduler/policy/orchestrator actors and the UI must not let a human impersonate them (the domain table would 403 anyway; the UI simply never offers the buttons).

### 5.2 Stage progress derivation (FR-UI-001)

```text
progress = position(state) in [preparing, coding, verifying, qa, quality_gate, pr_draft, completed]
queued → 0% "awaiting admission" · blocked/retry_wait → badge on parked stage ·
failed/cancelled → terminal badge, no percent
```

Pure function of `task_run_events` + current `state` — nothing stored, so it can never disagree with the state machine. `RunStateBadge` maps all 12 states to design-guidelines `MetricBadge` variants (success: completed; destructive: failed/cancelled; warning: blocked/retry_wait/queued; info: active stages; neutral: unknown).

### 5.3 Admission visibility (E2E-06)

`GET /operator/overview.admission` = `{max: 3, active: count(task_runs where state in preparing..pr_draft)}`, `queued` and `deferred_count` shown alongside — the ≤3 invariant is *displayed from live rows*, and a UI counter exceeding 3 would be a real bug signal, not a display quirk.

---

## 6. Frontend Design (`apps/web`)

### 6.1 Routes & data source mapping

| Route / component | Data |
|---|---|
| `/` PlatformStatus (existing) | `/health`, `/ready` (+`outbox` dep), `/api/v1/system/status`, `GET /operator/workers`, `GET /operator/overview` |
| `/runs` RunListPage | `GET /operator/overview` (histogram header) + `GET /operator/runs` (cursor-paginated table; client "Load more") |
| `/runs/[runId]` RunDetailPage | `GET /operator/runs/{id}` + `GET /registry/runs/{id}/events` (timeline) + SSE `?run_id=` live tail |
| `OutboxPanel` (on `/` and detail) | `GET /operator/outbox`, `GET /operator/outbox/dlq` |
| `OperatorActions` (detail page) | `POST /registry/runs/{id}/transitions` (`block`/`resume`/`cancel`) |
| `DlqPanel` redrive buttons | `POST /operator/outbox/dlq/{id}/redrive` |

### 6.2 New components (design-guidelines-conformant)

- `RunStateBadge` — `MetricBadge` variant map for the 12 states; label always present (never color-only).
- `StageProgressBar` — 6-segment linear rail (preparing→pr_draft) + completed cap; parked/terminal overlays; `tabular-nums` percent.
- `RunTimeline` — `role="log"` ordered list of `task_run_events` ascending by `version` (the audit cursor), showing action/actor/actor_type/state transition/reason from metadata; human actions (`actor_type='human'`) get the `operator_id` sub-label.
- `EvidencePanel` — commit SHA (mono, copyable), branch, `gate_verdicts` verdict + policy + evidence JSON (collapsed), `pull_requests` card (PR number, draft badge, `ci_status` MetricBadge, `pr_url` external link).
- `ErrorDispositionCard` — `error_code`/`error_message` + `retry_due_at` + `attempt` + last failed transition from timeline.
- `OperatorActions` — `<button>`s opening a confirm dialog with mandatory `reason` field (≥3 chars); posts `expected_version` from the loaded run; 409 `stale_version` ⇒ toast "run changed — refresh" + auto-refetch.
- `OutboxPanel`/`DlqTable`/`RedriveConfirm` — per-type lag numbers, DLQ rows with reason, redrive requires reason; result surfaces `operator_audit_log`-backed confirmation.
- `WorkerHeartbeatCard` — per worker: kind, queue, `active_runs`, `last_beat_at` age, `UP`/`DOWN` badge by staleness.

### 6.3 `lib/api.ts` additions

Typed fetchers (`fetchOperatorOverview`, `listRuns`, `getRunDetail`, `fetchOutboxStats`, `listDlq`, `redriveDlq`, `postTransition`) mirroring the existing style — `cache: "no-store"`, typed error returns `{data, error, latencyMs}`, **never `any`**. Token resolution order (§8 D3): `sessionStorage['vtx.operator_token']` (set via a `/settings` token field) → `NEXT_PUBLIC_API_AUTH_TOKEN` read-only fallback. Mutation calls add `X-Operator-Id` from `sessionStorage['vtx.operator_id']`.

### 6.4 Realtime wiring

The existing SSE `fetch` reader is extended, not replaced: `run_id` filter when on a detail page; on `task_run.state_changed.v1` the affected run refetches (event = invalidation signal, never a state patch — the DB remains the source of truth); `pull_request.opened.v1` refetches the detail aggregate; reconnect honors `Last-Event-ID` = last seen `outbox_id`. Pause/Clear semantics unchanged.

### 6.5 Honesty + a11y (guidelines §5/§7 enforced)

Skeletons during load; `stale` dimming >3×poll; `UNKNOWN` badge for unmeasured deps; `role="log"` on streams; focus rings on all controls; `prefers-reduced-motion`; no emoji; no raw connection strings or tokens rendered — `pr_url`, repo `owner/name`, SHA are the only external identifiers shown.

---

## 7. Test Matrix (mapped to SRS §9)

Evidence band: **VTX-13** — "Operational dashboard, authenticated controls, realtime event safety" + UC-06/UC-07 + E2E-07.

| ID | Test | Layer | SRS evidence |
|---|---|---|---|
| UI-01 | Every `/operator/*` route without token ⇒ 401; with scheduler token on redrive ⇒ 403 | HTTP integ | FR-UI-003 |
| UI-02 | `GET /events?token=abc` ⇒ 401 `credential_in_query`; `?api_key=`/`?auth=` same | HTTP integ | FR-UI-004 |
| UI-03 | SSE `?run_id=` filters server-side; `Last-Event-ID` resumes from outbox cursor without loss/dup | HTTP integ | FR-UI-004, NFR-REL-001 |
| UI-04 | Tailer never claims/ACKs: claimed_by/published_at untouched after poll | integ | OD-03, FR-EVT-004 |
| UI-05 | Overview counts == seeded rows; `admission.active ≤ 3` invariant displayed from live data | integ | FR-UI-001, E2E-06 |
| UI-06 | Heartbeat stale (>3×interval) ⇒ worker `down`; fresh ⇒ `up` | unit svc | FR-UI-001, E2E-03 |
| UI-07 | `/ready` `dependencies.outbox`: lag > threshold ⇒ `degraded`; DLQ growth ⇒ still `degraded`/`down` per rule | HTTP integ | NFR-OPS-001 |
| UI-08 | Operator `block`/`resume`/`cancel` via transitions endpoint ⇒ `task_run_events` row `actor_type='human'`, `metadata.operator_id` present; wrong actor token ⇒ 403 | HTTP+pgx integ | FR-UI-003, E2E-07 |
| UI-09 | `stale_version` on concurrent double-submit ⇒ second 409, exactly one audit row | HTTP integ (20-writer variant reuses NFR-CON-001 harness) | NFR-CON-001 |
| UI-10 | Redrive: dead-lettered event ⇒ `applied`, `dead_lettered_at` cleared, `redrive_count++`, `operator_audit_log` row (actor/operator_id/reason/outcome/request_id) | pgx integ | UC-07, E2E-07 |
| UI-11 | Redrive on non-dead-lettered id ⇒ 409 `not_dead_lettered`, audit `rejected` row | pgx integ | E2E-07 |
| UI-12 | Run detail aggregate contains task/run/events/worktree/PR/verdict in one payload; missing PR ⇒ `pull_request: null` not error | HTTP integ | FR-UI-002 |
| UI-13 | `X-Operator-Id` sanitized (≤64 chars, whitespace flattened) into audit metadata; absent ⇒ `operator_id: null` not error | HTTP integ | FR-UI-003, NFR-AUD-001 |
| UI-14 | FE unit: `RunStateBadge` covers all 12 states → label+variant; `StageProgressBar` mapping for each state incl. blocked/retry_wait overlay | vitest | FR-UI-001 |
| UI-15 | FE: mutation dialog requires reason; 409 shows refresh path; no auto-retry of mutation | vitest + RTL | FR-UI-003, NFR-CON-001 |
| UI-16 | FE: SSE client never appends credentials to URL (regression guard); reconnect sends `Last-Event-ID` | vitest | FR-UI-004 |
| UI-17 | Migration 00007 up/down on populated DB; `redrive_count` survives whichever migration owns it | migration test | NFR-REL-001 |
| UI-18 | No token/secret material in any response payload or audit row (grep-style assertion over response bodies in integ tests) | HTTP integ | NFR-SEC-001, NFR-AUD-001 |

---

## 8. Open Decisions

| # | Decision | Status | Rationale / owner |
|---|---|---|---|
| D1 | SSE feed via read-only outbox tailer (no claim/ACK) | **DECIDED** | Sidesteps unresolved OD-03 multi-consumer ACK; DB is the durable buffer; `Last-Event-ID`/`outbox_id` cursor gives lossless resume without per-consumer receipt design. |
| D2 | Operator mutations reuse the existing `transitions` endpoint | **DECIDED** | The state machine already enforces actor gating (403 for non-operator) and writes the audit row — duplicating it in a dashboard mutation API would fork audit truth (FR-UI-003, FR-REG-005). |
| D3 | MVP credential delivery: operator token in `sessionStorage` (entered via settings UI), `NEXT_PUBLIC_*` env as read-only fallback on private network | **DECIDED (MVP) + STAKEHOLDER (hardening)** | Keeps token out of query string/source (FR-UI-004, NFR-PRIV-001) without a BFF. Before any non-private deployment: move mutations behind a Next.js route-handler session or real IdP — owner: Platform Administrator (OD-06). |
| D4 | `X-Operator-Id` free-text subprincipal (no IdP yet) | **DECIDED (MVP)** | `task_run_events.actor` stays `operator`; `metadata.operator_id` + `operator_audit_log.operator_id` carry the human identity. STAKEHOLDER follow-up: SSO/IdP-backed principal — owner: Platform Administrator + Tech Lead. |
| D5 | Worker liveness via `worker_heartbeats` upsert (3×beat staleness) | **DECIDED** | Honest measurement (FR-UI-001): every loop that can hold a lease already has an identity to beat as; staleness computed at read, never stored. |
| D6 | `/ready` adds `dependencies.outbox` (lag threshold `READY_OUTBOX_LAG_WARN=5m`) | **DECIDED** | Extends the existing contract additively; threshold is env-configurable pending OD-06 SLO numbers — the *value* is a placeholder, the *mechanism* is decided. |
| D7 | Policy actor may read `/operator/*`, never mutate | **DECIDED** | Lets retry orchestration observe DLQ/overview without an operator credential; mutation routes check `actor == operator` explicitly. |
| D8 | Run list cursor on `(updated_at, run_id)` descending | **DECIDED** | Matches operator usage ("what moved lately"); stable under concurrent updates unlike `created_at`-only cursors (same reasoning as version-cursor on events). |
| D9 | Page cap 200 items (`OPERATOR_PAGE_MAX`) | **DECIDED** | Consistent with registry `maxPageLimit=200` already in `handlers_registry.go`. |
| D10 | Whether `/operator/*` also serves machine actors via a fifth "dashboard" token | **STAKEHOLDER** | Only needed if an external monitoring system must read this surface; today operator+policy coverage suffices. Owner: Tech Lead. |
| D11 | `stage_not_operational`-style honesty for CI evidence: dashboard shows `ci_status` verbatim; "unknown" renders `UNKNOWN` badge | **DECIDED** | Mirrors OD-05 posture — no inferred health claims (§7 Web interface rule). |
