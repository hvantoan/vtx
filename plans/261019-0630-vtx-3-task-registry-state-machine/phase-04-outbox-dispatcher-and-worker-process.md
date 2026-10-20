---
phase: 4
title: "Outbox Dispatcher & Registry Worker Process"
status: done
priority: P1
effort: "2d"
dependencies: [1, 3]
---

# Phase 4: Outbox Dispatcher & Registry Worker Process

## Goal
Dispatcher duy nhất tiêu thụ `execution_intent.created.v1` (claim → xử lý ngoài tx → ACK), chạy trong process `registry-worker` **độc lập Temporal**, kèm DLQ/redrive, backoff jitter và harness bằng chứng đa process.

## Files to Create / Modify
- Create: `apps/backend/internal/application/outbox_dispatcher.go`, `apps/backend/internal/application/outbox_dispatcher_test.go`
- Modify: `apps/backend/cmd/worker/main.go` — nhận mode qua `os.Args[1]`: rỗng/`temporal` = Temporal worker như hiện tại, `registry` = dispatcher (không gọi `client.Dial`)
- Modify: `infra/compose/docker-compose.yml`, `infra/compose/docker-compose.prod.yml` — service `registry-worker`: `command: ["/app/worker","registry"]`, **không** publish port, `depends_on: postgres: {condition: service_healthy}`, **không** phụ thuộc `temporal`. Dùng `environment:` dạng list liệt kê tường minh như service `backend` (`infra/compose/docker-compose.yml:64-84`) — **không** dùng `${DATABASE_URL}`: biến đó không tồn tại trong `ci.env`/compose, phải dùng đúng DSN `postgres://vtx_user:vtx_password@postgres:5432/vtx_platform?sslmode=disable` (như `backend`) và bổ sung `APP_ENV`; compose không tự truyền biến không được liệt kê.
- Create: `infra/scripts/test-registry-outbox.sh`
- Modify: `.github/workflows/ci.yml` — step harness trong job `backend-integration` + artifact `registry-evidence-${{ github.sha }}`

## Tasks & Steps
1. Mode split trong `cmd/worker/main.go`: registry mode không dial Temporal, có signal handling riêng (SIGTERM ⇒ drain + exit), log `slog` "registry dispatcher started" — TR-16 viết ở mức package nên `cmd/worker` vẫn được phép import Temporal.
2. Claim (tx ngắn, **commit trước mọi I/O**):
   `SELECT outbox_id, payload FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL AND event_type='execution_intent.created.v1' AND (claimed_until IS NULL OR claimed_until < now()) AND (next_attempt_at IS NULL OR next_attempt_at <= now()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT n` → set `claimed_by`, `claimed_until = now() + lease`, `attempt = attempt + 1` → COMMIT.
3. Xử lý ngoài transaction: decode + validate payload; gọi `ConsumeIntent` (tx riêng của Phase 3).
4. ACK ở **tx riêng**: `UPDATE outbox_events SET published_at = now() WHERE outbox_id=$1 AND claimed_by=$2`.
5. Lỗi: transient (DB/network) ⇒ `claimed_until = NULL`, `next_attempt_at = now() + min(base·2^attempt, cap) · jitter`; unsupported `event_type`/thiếu field/poison ⇒ `dead_lettered_at = now()` + `dead_letter_reason` (đã sanitize). Không ACK khi xử lý một phần.
6. Vòng lặp poll bounded + context cancellation; claim lease hết hạn tự được nhặt lại ⇒ crash/restart không mất pending.
7. Routing: lọc cứng `event_type='execution_intent.created.v1'` ⇒ event `task_run.*` không thể quay lại chính consumer này (không cần bảng route). Mọi query hỏi "còn việc"/lag phải kèm `AND dead_lettered_at IS NULL`.
8. `infra/scripts/test-registry-outbox.sh` (copy helper `ok`/`bad`/`note` từ `infra/scripts/test-jira-webhook.sh`, dùng cùng `PSQL`/`COMPOSE` pattern + `resolve_ports`):
   - TR-03: `--scale registry-worker=2` (không cần override ports vì service không publish port) → 1 run.
   - TR-08/TR-20: `docker compose kill -s SIGKILL registry-worker` giữa consume-tx và ACK → replay không nhân bản run/audit/outbound.
   - TR-12/TR-13/TR-22: poison + unsupported `event_type` → `dead_lettered_at` có reason; redrive bằng `UPDATE outbox_events SET dead_lettered_at=NULL, dead_letter_reason=NULL, attempt=0, next_attempt_at=NULL WHERE outbox_id=...` rồi xác nhận xử lý lại được.
   - Backfill/TR-24: bật dispatcher trên DB có intent pending ⇒ `count(*) WHERE published_at IS NULL` về 0, không cần job/checkpoint riêng.
9. CI: thêm step harness + artifact; không sửa job khác.
10. **Client trong harness**: runtime image backend là `alpine:3.21` + `ca-certificates tzdata` (`apps/backend/Dockerfile:18,20`) — **không có `curl`**. Khi bắn request vào replica (TR-07/TR-14) dùng busybox `wget -qO- --header=... --post-data=...` trong `docker exec`, hoặc one-shot `python:3.13-alpine` trên cùng compose network. Ghi đúng lệnh đã chọn vào evidence.

## Verification
```bash
docker compose --env-file ci.env -f infra/compose/docker-compose.yml -f infra/compose/docker-compose.e2e.yml \
  --profile e2e up -d --build registry-worker postgres
docker compose ... --scale registry-worker=2 up -d
./infra/scripts/test-registry-outbox.sh | tee registry-evidence.log
```
Kỳ vọng: sau khi worker chạy hết, `count(*) FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL AND event_type='execution_intent.created.v1'` = **0**; `--scale registry-worker=2` + cùng event ⇒ **1** run; kill -9 giữa tx ⇒ không nhân bản; poison ⇒ DLQ có reason và redrive được.

## Risks & Mitigations
- Temporal connect fail không được làm registry dispatcher exit ⇒ registry mode phải không khởi tạo Temporal client (đây là điều kiện nghiệm thu G3, không phải tuỳ chọn).
- Giữ DB lock khi gọi mạng ⇒ claim commit xong mới decode/consume.

## Todo
- [ ] `outbox_dispatcher.go` + mode `registry` trong `cmd/worker`
- [ ] Service `registry-worker` trong compose base + prod, không phụ thuộc Temporal
- [ ] Harness `test-registry-outbox.sh` cho TR-03/08/12/13/20/22 + backfill
- [ ] Wire CI + artifact `registry-evidence-${{ github.sha }}`

## Next Steps
Phase 5 thêm HTTP API đọc/mutation có auth theo actor trên cùng service.
