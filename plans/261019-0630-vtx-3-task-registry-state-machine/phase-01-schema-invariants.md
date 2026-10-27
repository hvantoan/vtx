---
phase: 1
title: "Schema, Compatibility Matrix & Invariants"
status: done
priority: P1
effort: "1.5d"
dependencies: []
---

# Phase 1: Schema, Compatibility Matrix & Invariants

## Goal
Nộp compatibility matrix (§0.A) **trước**, rồi giao schema registry additive — `registry_tasks`, `task_runs`, `task_run_events`, `deferred_intents`, `inbox_consumptions`, ALTER `outbox_events` — trên DB VTX-2 có dữ liệu, không DROP/rename bảng nào.

## Files to Create / Modify
- Create: `tests/evidence/vtx-3-compatibility-matrix.md` (final hoá Appendix A của advice), `tests/evidence/registry-migration-precheck.log`
- Create: `apps/backend/db/migrations/00004_task_registry.sql`
- Create: `apps/backend/db/queries/registry.sql`, `apps/backend/db/queries/outbox.sql`
- Regenerate: `apps/backend/internal/persistence/db/*` qua `sqlc generate`
- Create: `apps/backend/internal/persistence/db/registry_integration_test.go` (`//go:build integration`)
- Modify: `Makefile` (target `test-integration`), `.github/workflows/ci.yml` (step mới trong `backend-integration`, ngay sau bước migrations, tái dùng `$JT_PG_PORT`)

## Tasks & Steps
1. **Matrix + precheck trước khi viết migration.** Trên DB populated: `count(*)` của `tasks`, `task_events`, `agent_sessions`, `execution_intents`, `outbox_events WHERE published_at IS NULL`; liệt kê FK trỏ vào `tasks`; xác nhận 4 bảng VTX-2 chỉ được extend. Số liệu dev 2026-10-19: `tasks=0 task_events=0 agent_sessions=0 intents=1 pending=1`.
2. `00004` theo thứ tự: `registry_tasks` → `task_runs` → partial unique I-03 → `task_run_events` → `deferred_intents` → `inbox_consumptions` → `ALTER TABLE outbox_events`.
   - `registry_tasks`: `id UUID PK DEFAULT gen_random_uuid()`, `jira_site_id`, `jira_issue_id`, `jira_issue_key`, `project_key`, `version BIGINT NOT NULL DEFAULT 0`, `created_at/updated_at TIMESTAMPTZ NOT NULL DEFAULT now()`, `UNIQUE(jira_site_id, jira_issue_id)`.
   - `task_runs`: `run_id UUID PK DEFAULT gen_random_uuid()`, `task_id → registry_tasks`, `intent_id UUID NOT NULL UNIQUE → execution_intents`, `correlation_id`, `origin_event_id`, `changelog_id`, `state TEXT NOT NULL CHECK`, `version BIGINT NOT NULL DEFAULT 0`, `attempt INT NOT NULL DEFAULT 0`, `resume_state`, `retry_due_at`, `commit_sha TEXT NULL`, `branch`, `error_code`, `error_message`, `lease_owner`, `lease_epoch BIGINT NOT NULL DEFAULT 0`, `lease_expires_at`, `created_at/updated_at/started_at/finished_at`.
   - I-03: `CREATE UNIQUE INDEX ... ON task_runs(task_id) WHERE state NOT IN ('completed','failed','cancelled')`.
   - `task_run_events`: `event_id UUID PK DEFAULT gen_random_uuid()`, `run_id → task_runs`, `previous_state`, `current_state`, `action`, `actor`, `actor_type`, `correlation_id`, `version BIGINT NOT NULL`, `metadata JSONB`, `created_at`, `UNIQUE(run_id, version)`.
   - `deferred_intents`: `intent_id UUID PK → execution_intents`, `task_id → registry_tasks`, `received_at NOT NULL DEFAULT now()`, `disposition`, `reason`, `UNIQUE(intent_id)` + index FIFO `(received_at, intent_id)`.
   - `inbox_consumptions`: `consumer_name`, `source_event_id UUID`, `run_id NULL`, `consumed_at`, `UNIQUE(consumer_name, source_event_id)`.
   - ALTER `outbox_events`: `claimed_by TEXT`, `claimed_until TIMESTAMPTZ`, `attempt INT NOT NULL DEFAULT 0`, `next_attempt_at TIMESTAMPTZ`, `dead_lettered_at TIMESTAMPTZ`, `dead_letter_reason TEXT`.
3. Goose Up/Down đủ hai chiều. Dùng `DEFAULT gen_random_uuid()` (PG17 có sẵn, không cần pgcrypto) và lấy id bằng `RETURNING` — **không** import `github.com/google/uuid` (hiện là indirect trong `go.mod`; import trực tiếp sẽ sửa `go.mod`).
4. Queries: `registry.sql` (upsert task theo identity; insert run `RETURNING run_id`; select run `FOR UPDATE`; update run theo CAS; insert audit; insert deferred; insert receipt `ON CONFLICT DO NOTHING`; select deferred FIFO) + `outbox.sql` (claim `FOR UPDATE SKIP LOCKED`, mark published, attempt/backoff, quarantine, redrive). Cả hai file chạy trong `pgx.Tx` qua `queries.WithTx` như `db/queries/jira.sql`.
5. `sqlc generate` → commit code sinh ra, giữ `emit_interface: true`.
6. Bootstrap làn integration: build tag `integration`, DSN từ `TEST_DATABASE_URL`, target `make test-integration` (`go -C apps/backend test -tags integration -race ./...`), step CI mới sau bước migrations.
7. Tests: I-01, I-02, I-03 **qua 2 connection DB** (không phải 2 goroutine), I-05 (version=0 + audit version 0), TR-11 (up/down/up + FK/unique/index + sqlc drift), TR-18/TR-24 (DB populated: ID/row/outbox pending còn nguyên; chạy lại precheck sau migration).

## Verification
```bash
cd apps/backend && sqlc generate && git diff --exit-code internal/persistence/db
goose -dir apps/backend/db/migrations postgres "$DSN" up && goose ... down-to 0 && goose ... up
TEST_DATABASE_URL=postgres://vtx_user:vtx_password@localhost:49432/vtx_platform?sslmode=disable make test-integration
go -C apps/backend test -race ./...   # unit sạch; integration bị loại bởi build tag
```
Kỳ vọng: I-01/I-02/I-03/I-05 PASS; precheck sau migration bằng trước migration; `make test-backend` không chạm DB.

## Risks & Mitigations
- Nếu partial unique I-03 không diễn đạt được đúng ngữ nghĩa "admitted nonterminal", phải biết ngay ở đây (không phải ở TR-19): dùng transaction + advisory lock và ghi rõ trong evidence.
- DB populated chỉ có ở CI ⇒ precheck và up/down/up phải chạy trong job, artifact `registry-migration-precheck.log`.

## Todo
- [ ] Matrix + precheck committed vào `tests/evidence/`
- [ ] `00004` up/down/up sạch trên DB có dữ liệu; sqlc drift = 0
- [ ] I-01/I-02/I-03/I-05 PASS
- [ ] `make test-integration` chạy được trong CI

## Next Steps
Phase 2 dùng đúng `state`/`version`/`lease_epoch` định nghĩa ở đây.
