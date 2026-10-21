# 2026-10-19 — VTX-3 lập plan (Task Registry & State Machine)

## Bối cảnh
VTX-3 (AUTO-02, SPEC-AUTO-02 v1.1) là ticket `To Do` chặn VTX-4/5/7, nhưng lần này chỉ **lập plan**: brainstorm → contract → plan 7 phase. Repo `vtx` tại HEAD `85a7d9a`, branch `dev`.

## Việc đã làm
- Xác minh lại các claim nền bằng đọc file trực tiếp: compose không có service worker, `cmd/worker` chỉ dựng Temporal worker, `published_at` chưa từng được ghi (chỉ `INSERT … RETURNING`), chưa có test nào chạm DB thật, `make test-backend = go test -v -race ./...`, `BearerAuthMiddleware` so sánh hằng-thời.
- Chốt 7 quyết định với PO: bảng `registry_tasks`; dispatcher = mode `registry` trong `cmd/worker` với service compose `registry-worker`; 4 env token actor fail-closed; stage chưa vận hành chặn cứng (7 hop); `published_at` = ACK consumer VTX-3; §0.E = N replica cùng `consumer_name`; metrics = slog + query SQL.
- Precheck dữ liệu thật trên DB dev: `tasks=0 task_events=0 agent_sessions=0 execution_intents=1 outbox_events=1 (pending)` — bảng bootstrap rỗng, có sẵn 1 intent chưa publish ⇒ bật dispatcher chính là backfill.
- Tạo plan `plans/261019-0630-vtx-3-task-registry-state-machine/` (7 phase) qua `ak plan create/add-phase`; `ak plan validate` OK, status `0/7`.
- Red-team inline (delegation subagent không khả dụng: `No model selected`) tìm được 7 finding có evidence, trong đó 2 lỗi thật: TR-07/TR-14 không thể đi qua gateway (`location /` → 404, `e2e-scale` reset port) và runtime image không có `curl`.

## Bài học
- Advice doc bị sửa tại chỗ giữa phiên (DROP → PRESERVE) và có lúc tồn tại hai bản §10/§12 khác nhau; đọc lại file trước khi handoff thay vì tin bản đã đọc trước đó.
- Gate G2 cần artifact "subscriber receipt contract" ngay từ đầu, không phải mục "không chặn".

## Tiếp theo
`/ak:cook plans/261019-0630-vtx-3-task-registry-state-machine/plan.md` — bắt đầu Phase 1 (matrix + precheck trước khi viết `00004`).
