---
phase: 2
title: "Domain State Machine"
status: done
priority: P1
effort: "0.5d"
dependencies: [1]
---

# Phase 2: Domain State Machine

## Goal
Định nghĩa states/actions/actors, transition map đủ §0.C và typed error của registry trong `internal/domain` — thuần dữ liệu, test được không cần DB.

## Files to Create / Modify
- Create: `apps/backend/internal/domain/registry.go` (states, actions, actors, transition map, guards, typed errors)
- Create: `apps/backend/internal/domain/registry_test.go`
- Modify: (không) `apps/backend/internal/domain/models.go` chỉ khi cần dùng chung type — không đổi type hiện có

## Tasks & Steps
1. Machine values đúng chuỗi spec: `queued, preparing, coding, verifying, qa, quality_gate, pr_draft, completed, blocked, retry_wait, failed, cancelled`; terminal = `completed/failed/cancelled`; `blocked/retry_wait` là nonterminal có `resume_state`.
2. Transition map `map[transitionKey]transition` với `transitionKey{from, action}` (không DSL, không interface một implementation):
   - `queued+begin → preparing` (scheduler, guard: admission + lease hợp lệ)
   - `preparing/coding/verifying/qa/quality_gate + advance → stage kế tiếp`; `pr_draft+complete → completed`
   - `block` (nonterminal trừ `blocked/retry_wait`) → `blocked`, lưu `resume_state` + reason; `blocked+resume → resume_state`; `schedule_retry` → `retry_wait` + `resume_state` + `retry_due_at`; `retry_wait+retry_due → resume_state`; `cancel`/`fail` mọi nonterminal.
3. **Tập hop fail-closed ở Phase 1** (chốt PO #4) — map vẫn đủ §0.C, nhưng guard đóng:
   - `queued+begin`: guard thật (lease + admission), không thể đạt hợp lệ vì chưa có API cấp lease (VTX-5).
   - Toàn bộ `advance`/`complete` từ `preparing` trở đi: `preparing→coding`, `coding→verifying`, `verifying→qa`, `qa→quality_gate`, `quality_gate→pr_draft`, `pr_draft→completed`.
   - Vẫn mở: `block`, `resume`, `schedule_retry`, `retry_due`, `cancel`, `fail`.
   - Cài đặt bằng guard tường minh trả typed error (không hardcode deny rải rác); mã lỗi phải ổn định để test âm bắt được.
4. Actor → quyền: `scheduler`, `orchestrator`, `operator`, `policy`; action không thuộc actor ⇒ `forbidden`.
5. Typed errors đủ §0.D kèm HTTP status: `not_found`/404, `stale_version`/409, `stale_lease`/409, `already_terminal`/409, `invalid_transition`/409, `forbidden`/403, `conflict_active_run`/409.
6. Epoch guard hai nhánh tường minh (không `if expectedEpoch == 0 { skip }`): `(lease_owner IS NULL AND action thuộc policy operator trên queued/blocked) OR (lease_owner IS NOT NULL AND lease_epoch = $expected AND lease_expires_at > now())`.

## Verification
```bash
go -C apps/backend test -race ./internal/domain/...
```
Kỳ vọng: bảng transition khớp §0.C (test table-driven liệt kê toàn bộ hop hợp lệ + hop cấm); self-transition và mọi chuyển từ terminal ⇒ lỗi; mỗi hop trong tập fail-closed trả đúng typed error.

## Risks & Mitigations
- Dễ nới `begin` cho "chạy được" ⇒ mở lại G4. Guard `begin` phải là lease/admission thật, chỉ chứng minh bằng SQL-injected lease.
- Sai lệch chuỗi state so với spec ⇒ mọi test sau vô nghĩa: khoá bằng hằng số + test so khớp literal.

## Todo
- [ ] `registry.go` + `registry_test.go`
- [ ] Transition map đủ §0.C, terminal/nonterminal đúng
- [ ] Tập hop fail-closed đóng, có test âm từng hop
- [ ] Typed error ↔ HTTP status theo §0.D

## Next Steps
Phase 3 gọi map này trong `TransitionRun`.
