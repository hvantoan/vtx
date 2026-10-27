---
phase: 7
title: "Evidence & DoD Closure"
status: done
priority: P1
effort: "1d"
dependencies: [1, 2, 3, 4, 5, 6]
---

# Phase 7: Evidence & DoD Closure

## Goal
Đóng DoD của VTX-3: TR-01..TR-25 có evidence thật (PASS hoặc BLOCKED), CI xanh tại exact SHA, compatibility matrix + precheck đã nộp, deviation comment trên Jira.

## Files to Create / Modify
- Create: `tests/evidence/vtx-3-tr-matrix.md` (TR → lane → kết quả → artifact)
- Modify: `.github/workflows/ci.yml` — step `make test-integration`, step harness, artifact `registry-evidence-${{ github.sha }}` + `registry-migration-precheck.log` (cùng job `backend-integration`, giữ `timeout-minutes: 20` nếu còn đủ)
- Create: `tests/evidence/registry-integration.log`, `tests/evidence/registry-outbox.log` (artifact sinh ra từ CI/local)

## Tasks & Steps
1. Bảng TR theo làn (các TR còn lại nằm trong phase tương ứng đã nêu):

| TR | Lane | Nơi chạy |
|---|---|---|
| TR-01/02/04/05/06/09/10/15/17/19/21/25 | Go integration (`//go:build integration`) | `make test-integration` |
| TR-11 | CI migrate step | goose up/down-to 0/up + `sqlc generate` drift |
| TR-18/TR-24 | CI migrate step trên DB populated | precheck + up + so row counts/ID |
| TR-16 | unit | `go list -deps` (không có `go.temporal.io`) |
| TR-03/TR-07/TR-08/TR-12/TR-14/TR-20/TR-22 | shell harness | `infra/scripts/test-registry-outbox.sh` (+ `--scale backend=2` cho TR-07) |
| TR-13/TR-23 | integration + harness | payload sai schema; actor spoof qua HTTP |

   - **TR-07 đã chạy thật** bằng `cancel` + token `operator` (`expected_version=0`), không dùng `advance` như ví dụ dưới: `advance` là hop contract-only (`stage_not_operational`) và phase 1 chưa có allocator cấp lease (VTX-5), nên ví dụ gốc không thể đạt 200. Kết quả: 1×200 + 19×409, `version=1`, một audit row.
   - **TR-07 (20 writer × 2 instance) không đi qua gateway**: `location /` trả 404 (`infra/compose/nginx/default.conf.template:91`) và `docker-compose.e2e-scale.yml` reset port của `backend`, nên harness lấy từng container bằng `docker compose ps -q backend` rồi bắn song song vào cả hai replica **bằng busybox `wget`, không phải `curl`** — runtime image là `alpine:3.21` + `ca-certificates tzdata`, không có curl (`apps/backend/Dockerfile:18,20`):
     ```bash
     for id in $(docker compose ... ps -q backend); do
       docker exec "$id" wget -qO- \
         --header="Authorization: Bearer $REGISTRY_ACTOR_ORCHESTRATOR_TOKEN" \
         --header="Content-Type: application/json" \
         --post-data="{\"expected_version\":$V,\"expected_lease_epoch\":1,\"action\":\"advance\"}" \
         "http://localhost:49080/api/v1/registry/runs/$RUN/transitions"
     done
     ```
     (hoặc one-shot `python:3.13-alpine` trên cùng compose network — image đã dùng cho `jira-stub`: `infra/compose/docker-compose.e2e.yml:18-22` — nếu cần client mạnh hơn). ≥2 process thật, đúng tinh thần AC-05, không cần mở ingress mới.
   - **Harness phải start `registry-worker` tường minh**: step CI hiện tại chỉ `up -d --build backend gateway jira-stub postgres`, nên `test-registry-outbox.sh` thêm `registry-worker` vào danh sách service (cùng `-f infra/compose/docker-compose.yml -f infra/compose/docker-compose.e2e.yml --profile e2e`), và `--scale registry-worker=2` cho TR-03.

2. Chạy đủ protocol repo: `make test-boundaries && make vet && make typecheck && make test && make build`.
3. Ghi SHA thực tế vào evidence (`git rev-parse HEAD`); artifact phải khoá theo `${{ github.sha }}`.
4. Comment deviation trên VTX-3 (đã chốt với PO, ghi ngắn gọn kèm lý do): (a) bảng registry tên `registry_tasks`, spec §3 ghi literal `tasks`; (b) **không** DROP/rename 3 bảng bootstrap; (c) `published_at` = ACK của consumer VTX-3, query "còn việc"/lag lọc theo `event_type`, `task_run.*` giữ pending tới VTX-5; (d) §0.E = một consumer identity, N replica cùng `consumer_name`; (e) tập hop fail-closed ở Phase 1 (liệt kê 7 hop) và lý do; (f) metrics = slog + query SQL; (g) `begin` và `retry_due` chỉ chứng minh bằng SQL-injected lease; (h) thứ tự kiểm tra lỗi đặt `invalid_transition` trước `stale_lease` (classifier reorder, §0.D ghi ngược lại) + thêm class `stage_not_operational`/409; (i) lease: §0.D đọc literal "(if an active lease exists)" — không có lease active thì policy/operator action được phép từ mọi state nonterminal (queued/blocked chỉ là pre-lease điển hình); có lease mà sai owner/epoch/hết hạn thì stale_lease; (j) backoff = `min(base·2^attempt, cap 5m)` + jitter, nhóm theo `attempt` trong một batch; (k) `task_id` đã thêm vào payload `task_run.intent_deferred.v1`; (l) bỏ endpoint `GET /tasks/{id}/runs` (không có trong danh sách read của §7, cursor theo `version` mutable sẽ trượt trang).
5. TR nào không tạo được môi trường (2 replica, DB populated, DLQ) ⇒ ghi **BLOCKED** kèm lý do và cách dựng môi trường — không hạ chuẩn test, không PASS hình thức.
6. Cập nhật `tests/evidence/vtx-3-compatibility-matrix.md` nếu migration thực tế lệch bản nháp Phase 1.

## Verification
```bash
git rev-parse HEAD
make test-boundaries && make vet && make typecheck && make test && make build
make test-integration
./infra/scripts/test-registry-outbox.sh | tee tests/evidence/registry-outbox.log
gh run view --json headSha,conclusion   # hoặc đọc URL job backend-integration
```
Kỳ vọng: toàn bộ target xanh; job `backend-integration` xanh tại exact SHA; artifact `registry-evidence-<sha>` chứa log integration + harness + precheck; bảng TR không còn ô trống.

## Risks & Mitigations
- Job `backend-integration` có `timeout-minutes: 20` ⇒ harness mới phải nhanh (dùng lại stack sẵn có, không `--build` lặp lại); nếu vượt, tách step nhưng vẫn cùng job/artifact.
- Claim PASS khi mới chỉ chạy 1 process ⇒ đây đúng là thứ TR-03/TR-07 kiểm: bắt buộc `--scale`.

## Todo
- [x] `tests/evidence/vtx-3-tr-matrix.md` điền đủ TR-01..25
- [x] CI: `make test-integration` + harness + artifact theo SHA — step/artifact đã thêm; **xanh tại SHA BLOCKED** vì tree chưa commit
- [x] `make test-boundaries && make vet && make typecheck && make test && make build` xanh (local)
- [x] Deviation comment trên Jira VTX-3 — đăng 2026-10-19, comment `10019`
- [x] Không còn TR nào ở trạng thái chưa xác định (PASS/BLOCKED)

## Next Steps
Reviewer signoff → merge một lần khi DoD đủ (D3).
