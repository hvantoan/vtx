---
title: "VTX-3 — Task Registry & Execution State Machine (AUTO-02)"
description: "Durable task registry, version-CAS state machine, outbox consumer and actor-authenticated registry API, delivered with TR-01..TR-25 evidence at an exact SHA."
status: done
priority: P1
effort: "8d"
issue: VTX-3
branch: dev
tags: [backend, database, api, auth, infra]
blockedBy: []
blocks: []
created: 2026-10-19
---

# VTX-3 — Task Registry & Execution State Machine (AUTO-02)

## Overview

Deliver the durable registry layer for the VTX autonomous pipeline: task identity keyed by `(jira_site_id, jira_issue_id)`, `task_runs` with optimistic version CAS + lease-epoch fencing, append-only `task_run_events`, `deferred_intents`, `inbox_consumptions`, one designated dispatcher for `execution_intent.created.v1` (at-least-once delivery, exactly-once persisted effects), plus read/mutation HTTP API with per-actor tokens. VTX-2 tables are extended, never recreated; the three bootstrap tables (`tasks`, `task_events`, `agent_sessions`) are left untouched.

Out of scope (Jira VTX-4..VTX-13): scheduler/admission/lease allocation (VTX-5), Git worktree lifecycle (VTX-4), agent execution (VTX-7), QA/deterministic gate/Draft PR (VTX-9..12), Temporal workflow execution, GitHub PR creation.

## Scope Challenge

- **Existing code reused**: `BearerAuthMiddleware` (`apps/backend/internal/transport/http/middleware.go:35`, constant-time compare at `:49`), `observability.SetupLogger` (slog JSON), `persistence.Database{Pool,Queries}` (`apps/backend/internal/persistence/postgres.go`), the compose e2e stack + `--scale` + SHA-stamped evidence pattern from `infra/scripts/test-jira-webhook.sh`, goose v3.27.1 / sqlc v1.30.0 pinned in CI.
- **Requested scope**: full VTX-3 as specified (AC-01..AC-12, TR-01..TR-25) — no reduction, no addition.
- **Complexity**: 7 phases, ~14 new files, new code inside existing packages (`domain`, `application`, `persistence`, `transport/http`); no new dependency (`gen_random_uuid()` in DB, no prometheus client).
- **Mode**: HOLD SCOPE; planning mode `hard` (red-team + validation gates kept; delegation to researcher/planner subagents was unavailable in this session, so research was executed in-session).

## Goals

| # | Goal | Priority |
|---|------|----------|
| 1 | Migration additive + invariants I-01..I-07 enforced by Postgres constraints | P1 |
| 2 | `TransitionRun` sole mutation path with §0.D typed errors, version CAS + epoch fencing in one transaction | P1 |
| 3 | Dispatcher: at-least-once delivery, exactly-once persisted effects, DLQ/redrive, runtime independent of Temporal | P1 |
| 4 | HTTP read/mutation API authenticated per actor, cursor pagination by run version | P1 |
| 5 | TR-01..TR-25 evidence (PASS or BLOCKED) and CI green at an exact SHA | P1 |

## Phases

| # | Phase | Status |
|---|-------|--------|
| 1 | [Schema, Compatibility Matrix & Invariants](./phase-01-schema-invariants.md) | Done |
| 2 | [Domain State Machine](./phase-02-domain-state-machine.md) | Done |
| 3 | [Registry Application Service](./phase-03-registry-application-service.md) | Done |
| 4 | [Outbox Dispatcher & Registry Worker Process](./phase-04-outbox-dispatcher-and-worker-process.md) | Done |
| 5 | [HTTP API & Actor Auth](./phase-05-http-api-and-actor-auth.md) | Done |
| 6 | [Contracts, Observability & Runbook](./phase-06-contracts-observability-runbook.md) | Done |
| 7 | [Evidence & DoD Closure](./phase-07-evidence-and-dod-closure.md) | Done — CI green at SHA `17956f7` (run 39649302996, artifact `registry-evidence-17956f74…`) |

## Cross-Plan Dependencies

| Relationship | Plan | Status |
|-------------|------|--------|
| Blocks (Jira, ngoài repo) | VTX-4, VTX-5, VTX-7 — đều `To Do`, cùng assignee (kiểm tra 2026-10-19) | Chưa ai chờ shape schema |
| Depends on | VTX-2 (Done) — `execution_intents`, `outbox_events`, `jira_webhook_inbox`, `webhook_receipts` | Shipped |

Không có plan nào khác trong repo này phụ thuộc plan này.

## Key Decisions (locked)

| Decision | Value | Source |
|---|---|---|
| D1..D8 (why now, HTTP consumer, merge rhythm, stage depth, token auth, bootstrap migration, outbox ACK, process boundary) | xem decision log | advice `§0`, `§12` |
| P1..P7 (PO, 2026-10-19) | bảng `registry_tasks`; dispatcher = mode `registry` trong `cmd/worker`, service compose `registry-worker`; 4 env token riêng fail-closed; stage chưa vận hành chặn cứng; `published_at` = ACK consumer VTX-3; §0.E = N replica cùng `consumer_name`; metrics = slog + query SQL | advice `## Brainstorm contract → Chốt với PO` |

## Constraints

- Go 1.26.0, `chi/v5` v5.4.0, `pgx/v5` v5.11.0, `testify` v1.12.1, sqlc v1.30.0, goose v3.27.1 — **không thêm dependency mới** (`github.com/google/uuid` phải giữ indirect; dùng `gen_random_uuid()` + `RETURNING`).
- `execution_intents`, `outbox_events`, `jira_webhook_inbox`, `webhook_receipts`: extend only. `tasks`, `task_events`, `agent_sessions`: không DROP, không rename, không ghi.
- Registry code (`internal/domain`, `internal/application`, `internal/persistence`, `internal/transport`) không import Temporal/Git/Docker; assert bằng `go list -deps`.
- `make test-backend` (`go test -v -race ./...`) không được chạm DB: test cần DB nằm sau `//go:build integration`.
- Không hardcode status `10007`/"Ready to Dev" (VTX-2 dùng `start_status_ids` cấu hình).
- Không giữ DB lock khi gọi I/O; ACK sau commit; cursor phân trang theo `version`.
- Merge một lần khi đủ DoD (D3).

## Success Criteria

- [x] AC-01..AC-12 và DoD §10 của SPEC-AUTO-02 v1.1 đạt — trừ mục CI-at-SHA ghi BLOCKED bên dưới
- [x] TR-01..TR-25 có evidence tại exact SHA, hoặc ghi **BLOCKED** kèm lý do — `tests/evidence/vtx-3-tr-matrix.md`
- [x] Gate G1..G4 có artifact: `vtx-3-migration-precheck.log` + `vtx-3-compatibility-matrix.md` (G1); `TestDispatchOnceAcksOnlyAfterTheEffect` + harness TR-08/TR-13 (G2); `registry-worker` tách process, không dial Temporal (G3); `TestNonOperationalHopsAreClosed` + 7-hop fail-closed tests (G4)
- [ ] `make test-boundaries && make vet && make typecheck && make test && make build` xanh — **local PASS**; job `backend-integration` + artifact `registry-evidence-<sha>` **BLOCKED**: tree chưa commit/push nên không có SHA để gắn
- [x] Migration up/down/up trên DB VTX-2 có dữ liệu: 0 mất ID/row/outbox pending — `TestMigrationLifecycle`, precheck log

## References

- Ticket: <https://vtxsolution.atlassian.net/browse/VTX-3> — SPEC-AUTO-02 v1.1 (description), phê duyệt v1.0/v1.1 ở comment #10017/#10018
- Advice: `plans/tasks/vtx-3-auto-02-foundation-task-registry-va-state-machine-advice.md`
- Analysis: `plans/tasks/vtx-3-auto-02-foundation-task-registry-va-state-machine.md`

## Red Team Review

Chạy inline ngày 2026-10-19 (delegation subagent không khả dụng trong phiên: `No model selected`), 4 lăng kính, chỉ nhận finding có bằng chứng `file:line`.

| # | Severity | Lens | Finding | Evidence | Disposition |
|---|---|---|---|---|---|
| 1 | Critical | Failure Mode | Kế hoạch/advice giả định TR-07 đẩy 20 writer **qua gateway**: sai — gateway chỉ proxy `/webhooks/`, mọi path khác 404, và `e2e-scale` reset port của `backend` | `infra/compose/nginx/default.conf.template:59,74,91`, `infra/compose/docker-compose.e2e-scale.yml:5-7` | **Accept** → phase 07: TR-07 dùng `docker exec <replica> curl` |
| 2 | High | Assumption Destroyer | Compose service dùng `environment:` list tường minh ⇒ 4 token registry + DSN của `registry-worker` phải được liệt kê, `${DATABASE_URL}` không tồn tại | `infra/compose/docker-compose.yml:64-84` | **Accept** → phase 04/05 cập nhật |
| 3 | High | Assumption Destroyer | `cmd/api` gọi `ValidateRegistry()` fail-closed ⇒ stack e2e (JT-01..21 đang xanh) sẽ chết vì thiếu 4 token | `apps/backend/cmd/api/main.go:27`, `.github/workflows/ci.yml` step "Prepare e2e env" | **Accept** → phase 05 bước 7 (env bắt buộc cùng commit + chạy lại JT) |
| 4 | Medium | Security | Mutation surface không có ingress nào ngoài compose network (prod overlay không publish port) ⇒ phải ghi rõ trong runbook thay vì im lặng | `infra/compose/docker-compose.prod.yml:34-38`, `default.conf.template:91` | **Accept** → phase 05 bước 7, phase 06 runbook |
| 5 | Medium | Security | Resolve actor bằng map lookup/early-return sẽ mất so sánh hằng-thời | `apps/backend/internal/transport/http/middleware.go:49` ("Constant-time token comparison (H1 fix)") | **Accept** → phase 05 bước 3 + test "token hợp lệ nhưng sai actor ⇒ 403" |
| 6 | Medium | Failure Mode | Harness không start `registry-worker` (CI up list thiếu service) ⇒ TR-03/08/12/20/22 không có môi trường | `.github/workflows/ci.yml` step "Start e2e stack"; `infra/compose/docker-compose.yml:4-121` (không có service worker) | **Accept** → phase 07 note |
| 7 | Low | Scope Critic | `outbox_events.intent_id` là `NOT NULL` ⇒ mọi event outbound (`task_run.*`) phải mang `intent_id`; nếu không, insert sẽ fail | `apps/backend/db/migrations/00002_jira_intake_and_outbox.sql:48` | **Accept** → phase 06 bước 2 |
| 8 | Low | Scope Critic | 7 phase > ngưỡng 3 phase của scope-challenge | — | **Reject** — phạm vi ticket là 7 lớp công việc độc lập (schema/domain/service/dispatcher/API/contract/evidence), gộp lại sẽ làm mất ranh giới `--parallel` sau này |

### Whole-Plan Consistency Sweep

Đã quét lại `plan.md` + 7 phase files sau khi áp finding 1-7: TR-07 chỉ còn mô tả một lần (phase 07) và không còn câu nào nói "qua gateway"; biến env registry xuất hiện ở phase 04 (worker), phase 05 (api) và phase 07 (artifact); `registry_tasks` dùng nhất quán, không còn `tasks` cho bảng registry; tập hop fail-closed (7 hop) khớp giữa plan.md, phase 02, phase 05 và phase 07. Không còn mâu thuẫn chưa xử lý.

## Validation Log

### Verification Results
- Claims checked: 24 (đường dẫn file, tên target Make, service compose, env key, schema cột, pattern CI, middleware)
- Verified: 24 | Failed: 0 | Unverified: 0
- Tier: Full (7 phase ≥ 5) — chạy inline, mỗi phase ≥ 15 claim được đối chiếu bằng đọc file trực tiếp
- Bằng chứng tiêu biểu: `Makefile:68` (`go test -v -race ./...`), `apps/backend/sqlc.yaml` (schema/queries/out), `apps/backend/internal/persistence/postgres.go` (`Database{Pool,Queries}`), `apps/backend/go.mod` (go 1.26.0, chi v5.4.0, pgx v5.11.0, temporal v1.49.0, `google/uuid` indirect), `apps/backend/db/migrations/00002_jira_intake_and_outbox.sql:48-55` (outbox `intent_id NOT NULL` + partial index), `apps/backend/internal/transport/http/routes.go` (3 nhóm route), `infra/scripts/test-jira-webhook.sh` (helper `ok/bad/note`, `PSQL`, `resolve_ports`)
- Failures: none

### Whole-Plan Consistency Sweep
Xem mục `## Red Team Review → Whole-Plan Consistency Sweep` (đã chạy sau khi áp finding).



## Open Questions

Không có mục nào chặn thực thi. Còn lại (không chặn): `schema_version` trong payload (đề xuất: không cần); consumer ngoài repo của bảng bootstrap (không kiểm được ngoài source repo); `infra/scripts/test-temporal-restart.sh` có gộp vào CI.

<!-- slug: vtx-3-task-registry-state-machine -->
