---
phase: 3
title: "Registry Application Service"
status: done
priority: P1
effort: "1.5d"
dependencies: [1, 2]
---

# Phase 3: Registry Application Service

## Goal
Cài `TransitionRun` (đường mutation duy nhất) và `ConsumeIntent` (đường tiêu thụ `execution_intent.created.v1`) — mỗi cái đúng **một** DB transaction, đúng thứ tự lỗi §0.D.

## Files to Create / Modify
- Create: `apps/backend/internal/application/registry_ports.go` (`RegistryRepository`, `EventPublisher`/`OutboxConsumer`, `Clock`, `LeaseValidator`)
- Create: `apps/backend/internal/application/registry_service.go` (`TransitionRun`, `ConsumeIntent`, typed errors)
- Create: `apps/backend/internal/persistence/registry.go` (adapter pgx/sqlc cho ports trên)
- Create: `apps/backend/internal/application/registry_service_test.go` (unit, không DB)
- Create: `apps/backend/internal/application/registry_consume_integration_test.go` (`//go:build integration`)

## Tasks & Steps
1. Ports theo §7 (interface nhỏ, một implementation mỗi cái — không factory, không abstraction thừa).
2. `TransitionRun(ctx, runID, expectedVersion, expectedLeaseEpoch, action, actor, metadata)` trong **một** tx:
   1. `SELECT state, version, lease_owner, lease_epoch, lease_expires_at FROM task_runs WHERE run_id=$1 FOR UPDATE`
   2. phân loại lỗi đúng thứ tự: `not_found` → `stale_version` → `stale_lease` → `already_terminal` → `invalid_transition`/`forbidden` (map từ Phase 2)
   3. `UPDATE task_runs ... WHERE run_id=$1 AND version=$2` (CAS, check rows affected)
   4. `INSERT task_run_events` (version+1) + `INSERT outbox_events` (`task_run.state_changed.v1`)
   5. commit
   `FOR UPDATE` chính là thứ làm TR-07 tất định: người đầu commit `version+1`, 19 người còn lại block rồi đọc version mới ⇒ `stale_version`.
3. Epoch guard hai nhánh tường minh (như Phase 2 §6) — không có nhánh `expectedEpoch == 0` bỏ qua check.
4. `run_id`/`event_id` lấy bằng `RETURNING ... gen_random_uuid()`; **không** import `github.com/google/uuid`.
5. `ConsumeIntent(ctx, evt)` trong **một** tx: validate payload (10 field bắt buộc; thiếu ⇒ typed schema/version error, không sinh giá trị thay thế, không hardcode status 10007) → `inbox_consumptions` insert `ON CONFLICT DO NOTHING` → upsert `registry_tasks` theo `(jira_site_id, jira_issue_id)` (`jira_issue_key` chỉ là metadata đổi được) → nếu không còn run nonterminal: insert run `queued` v0 + audit `version=0` + outbound `task_run.created.v1`; nếu còn: insert `deferred_intents` disposition `pending_deferred` + reason + outbound `task_run.intent_deferred.v1`.
6. **Không** set `published_at` ở đây (ACK thuộc Phase 4, tx riêng) — giữ ACK tách khỏi tx consume để TR-20 kiểm được crash-after-commit-before-ACK.
7. Không gọi mạng, không import Temporal/Git/Docker trong `internal/application` + `internal/domain`.

## Verification
```bash
go -C apps/backend test -race ./internal/application/...           # unit: fake clock/repo
TEST_DATABASE_URL=... make test-integration                        # integration: tx thật
```
Kỳ vọng:
- 20 goroutine cùng `expectedVersion` → **1** commit, **19** `stale_version`.
- Invalid/terminal transition → 0 mutation trên `task_runs` + `task_run_events` + `outbox_events`.
- Tx failure inject giữa chừng → rollback toàn bộ (TR-17, TR-25).
- `ConsumeIntent` ×3 cùng event → 1 run + 1 outbound; 2 intent khác nhau cùng issue → 1 admitted + 1 deferred, FIFO `(received_at, intent_id)` (TR-02, TR-10, TR-19, TR-20, AC-08).
- `commit_sha` NULL sau intake (AC-02).

## Risks & Mitigations
- CAS thuần `UPDATE ... WHERE version=$2` không phân biệt được 5 loại lỗi §0.D ⇒ bắt buộc `SELECT ... FOR UPDATE` trước.
- Nếu đặt ACK trong cùng tx consume thì TR-20 mất nghĩa ⇒ giữ hai tx tách biệt.

## Todo
- [ ] Ports + adapter pgx/sqlc
- [ ] `TransitionRun` FOR UPDATE → CAS → audit+outbox atomic
- [ ] `ConsumeIntent` task upsert → run | deferred → receipt → outbound
- [ ] Test goroutine 20 writer; test rollback; test replay ×3

## Next Steps
Phase 4 gọi `ConsumeIntent` từ dispatcher và ACK ở tx riêng.
