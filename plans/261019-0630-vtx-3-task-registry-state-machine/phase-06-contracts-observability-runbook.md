---
phase: 6
title: "Contracts, Observability & Runbook"
status: done
priority: P2
effort: "0.5d"
dependencies: [4, 5]
---

# Phase 6: Contracts, Observability & Runbook

## Goal
Đóng băng contract event đã phát, log/metric quan sát được bằng slug có cấu trúc, và runbook vận hành (redrive, backfill, giới hạn Phase 1).

## Files to Create / Modify
- Modify: `contracts/events/v1/events.json` — thêm `task_run.created.v1`, `task_run.state_changed.v1`, `task_run.intent_deferred.v1` vào `PlatformEvent.type` enum + definition payload tương ứng
- Create: `docs/runbook-task-registry.md` — runbook + sơ đồ state machine (mermaid) + schema notes + query vận hành
- Modify: call-site logging trong `internal/application/*` và `internal/transport/http/*` (dùng `slog` đã có ở `internal/observability/logger.go`, không tạo logger mới)
- Modify: `.github/workflows/ci.yml` — bước JSON schema đã có sẵn ở job `contract-conformance`; không thêm job

## Tasks & Steps
1. Event types versioned theo hậu tố (không thêm field `schema_version` vào payload — chốt PO §9.4): consumer từ chối `event_type` lạ ⇒ DLQ.
2. Payload outbound: `task_run.created.v1` (run_id, task_id, intent_id, jira_site_id, jira_issue_id, issue_key, state=queued, version=0, correlation_id, occurred_at) và `task_run.state_changed.v1` (run_id, previous_state, current_state, action, actor, actor_type, version, correlation_id, occurred_at) và `task_run.intent_deferred.v1` (intent_id, task_id, disposition, reason, received_at). **Mọi event outbound phải set `outbox_events.intent_id`** — cột này là `NOT NULL REFERENCES execution_intents` từ VTX-2 (`apps/backend/db/migrations/00002_jira_intake_and_outbox.sql:48`), lấy từ `task_runs.intent_id`; thiếu nó thì insert fail ngay.
3. Log (`slog`, JSON đã có): mọi log của registry mang `correlation_id`, `intent_id`, `run_id`, `event_id`, `error_category` (sanitized); thêm counter dạng log cho runs created, transitions success/conflict/invalid/stale, active runs by state, outbox lag/retry/DLQ, duplicate receipt. **Không** log token hay raw webhook body.
4. Metrics Phase 1 = slug line + 2 query SQL (chốt PO #7, không thêm `prometheus/client_golang`):
   - active runs by state: `SELECT state, count(*) FROM task_runs WHERE state NOT IN ('completed','failed','cancelled') GROUP BY state`
   - outbox lag: `SELECT count(*), min(created_at) FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL AND event_type='execution_intent.created.v1'`
5. Runbook `docs/runbook-task-registry.md` gồm: sơ đồ state (§0.C + tập hop fail-closed Phase 1), redrive SQL (`UPDATE outbox_events SET dead_lettered_at=NULL, dead_letter_reason=NULL, attempt=0, next_attempt_at=NULL WHERE outbox_id=$1`), backfill (= bật `registry-worker`, `published_at IS NULL` là checkpoint, không cần job riêng), ghi chú "Phase 1 chưa có consumer downstream cho `task_run.*`", "không có API cấp lease — `begin` chỉ chứng minh được bằng SQL test (VTX-5 sở hữu)", và cảnh báo mọi query "còn việc" phải kèm `AND dead_lettered_at IS NULL`.
6. Secret hygiene: bổ sung check trong `infra/scripts/test-registry-outbox.sh` kiểu JT-17 — grep log compose không chứa 4 token registry.

## Verification
```bash
node -e "JSON.parse(require('fs').readFileSync('contracts/events/v1/events.json'))"
npx @redocly/cli lint contracts/openapi/v1/openapi.yaml
# runbook: chạy đúng 2 query metric + câu redrive SQL trên DB e2e, dán kết quả vào evidence
```
Kỳ vọng: schema JSON hợp lệ, enum có 4 event type; redocly sạch; 2 query chạy được và trả số liệu thật; không token nào xuất hiện trong log.

## Risks & Mitigations
- Metric chỉ nằm trong doc mà không chạy được ⇒ phải thực thi query trong CI/harness và lưu output.
- Thêm prometheus client là dependency mới ngoài stop-and-ask ⇒ không làm ở Phase 1 (thêm sau là additive).

## Todo
- [ ] `events.json` +3 event type kèm definition
- [ ] slog có đủ field tương quan; không log token/payload thô
- [ ] Runbook có redrive/backfill/metric query chạy được
- [ ] Secret-hygiene check trong harness

## Next Steps
Phase 7 gom toàn bộ artifact và đóng DoD.
