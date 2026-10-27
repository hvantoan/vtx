---
phase: 5
title: "HTTP API & Actor Auth"
status: done
priority: P1
effort: "1d"
dependencies: [2, 3]
---

# Phase 5: HTTP API & Actor Auth

## Goal
Mở read + mutation API cho registry: actor lấy từ token đã xác thực (không lấy từ body), cursor phân trang theo `version`, lỗi typed map đúng HTTP status §0.D.

## Files to Create / Modify
- Create: `apps/backend/internal/transport/http/handlers_registry.go`, `handlers_registry_test.go`
- Modify: `apps/backend/internal/transport/http/middleware.go` (thêm `RegistryActorMiddleware`)
- Modify: `apps/backend/internal/transport/http/routes.go` (group mới `/api/v1/registry`)
- Modify: `apps/backend/internal/config/config.go` (4 field token actor + `ValidateRegistry()`)
- Modify: `apps/backend/cmd/api/main.go` (gọi `cfg.ValidateRegistry()` tại chỗ dựng registry router)
- Modify: `contracts/openapi/v1/openapi.yaml` (paths + bearer security)
- Modify: `.github/workflows/ci.yml` (thêm 4 biến vào heredoc `ci.env`), `.env.example`

## Tasks & Steps
1. Config: `REGISTRY_ACTOR_SCHEDULER_TOKEN`, `REGISTRY_ACTOR_ORCHESTRATOR_TOKEN`, `REGISTRY_ACTOR_OPERATOR_TOKEN`, `REGISTRY_ACTOR_POLICY_TOKEN` đọc bằng `getEnv(key, "")`.
2. `ValidateRegistry()` **riêng** (không sửa `Validate()`): cả 4 phải non-empty; khác `API_AUTH_TOKEN` và `GATEWAY_INTERNAL_AUTH_TOKEN`; 4 giá trị đôi một khác nhau; lỗi nêu đích danh biến thiếu. Lý do: `Validate()` chỉ được gọi ở `cmd/api/main.go:27` ⇒ nhét token vào đó vừa bắt mọi binary/test fixture phải có token registry, vừa không fail-closed cho dispatcher.
3. `RegistryActorMiddleware(tokens map[Actor]string)`:
   - Parse `Authorization: Bearer <token>` như `BearerAuthMiddleware` (`middleware.go:35`).
   - So sánh **hằng-thời** với **cả 4** token bằng `subtle.ConstantTimeCompare` như `middleware.go:49` (repo ghi "H1 fix") — **không** tra map theo token kèm early-return: đó là hồi quy bảo mật âm thầm và test 401/403 không bắt được.
   - Chỉ sau khi đã so hết mới chọn actor; không khớp ⇒ 401. Actor vào `context` dưới dạng principal.
4. Handlers:
   - Mutation: `POST /api/v1/registry/runs/{run_id}/transitions` body `{expected_version, expected_lease_epoch, action, reason, metadata}`. Body **không** có field `actor`; nếu client gửi `actor` khác principal ⇒ 403.
   - Map typed error → HTTP: `not_found` 404, `stale_version`/`stale_lease`/`already_terminal`/`invalid_transition`/`conflict_active_run` 409, `forbidden` 403.
   - Read: task theo `(jira_site_id, jira_issue_id)`, run theo `run_id`, history phân trang cursor **theo `version`** (`?after_version=N&limit=`), status. Không dùng `created_at` làm cursor (TR-23: stable khi timestamp bằng nhau).
5. Routes: `r.Group` mới trong `Routes()` với `RegistryActorMiddleware`; không đụng group dashboard (`BearerAuthMiddleware(s.cfg.AuthToken)`) và group webhook (`GatewayAuthMiddleware`).
6. openapi: thêm 4 path + `components.securitySchemes.bearerAuth`; chạy `npx @redocly/cli lint contracts/openapi/v1/openapi.yaml`.
7. **Env + ingress prerequisites (bắt buộc cùng commit, nếu không sẽ phá harness JT-01..21 đang xanh):**
   - Thêm 4 token vào `environment:` list của service `backend` trong `infra/compose/docker-compose.yml:64-84` (compose chỉ truyền biến được liệt kê), vào heredoc `ci.env` của job `backend-integration` (`.github/workflows/ci.yml`, step "Prepare e2e env"), và vào `.env.example`.
   - `cmd/api` gọi `ValidateRegistry()` khi dựng registry router ⇒ stack e2e khởi động api sẽ fail-closed nếu thiếu token. Chạy lại harness JT để chứng minh không hồi quy.
   - **Ingress**: gateway nginx chỉ proxy `location ^~ /webhooks/` → `/api/v1/integrations/jira/webhooks`, `location /` trả 404 (`infra/compose/nginx/default.conf.template:59,74,91`) ⇒ `/api/v1/registry/*` **không** đi qua gateway. Đây là chủ ý (mutation surface chỉ trong compose/k8s network; prod overlay không publish port cho `backend`), nhưng phải ghi rõ trong runbook: caller Phase 1 là process nội bộ (VTX-5 scheduler sau này), không có đường public.

## Verification
```bash
go -C apps/backend test -race ./internal/transport/... ./internal/config/...
npx @redocly/cli lint contracts/openapi/v1/openapi.yaml
# smoke thủ công:
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:49080/api/v1/registry/runs/$RUN/transitions -d '{}'   # 401
# + 4 token hợp lệ nhưng sai actor => 403; đúng actor + sai version => 409 stale_version
```
Kỳ vọng: 401 khi thiếu/sai token; 403 khi token hợp lệ nhưng actor không có quyền, và khi body `actor` khác principal; cursor theo version ổn định khi `created_at` bằng nhau; negative test toàn bộ tập hop fail-closed (Phase 2) ⇒ lỗi + **0 mutation / 0 audit row**.

## Risks & Mitigations
- Resolve actor bằng map lookup/early-return ⇒ mất so sánh hằng-thời. Bắt buộc so 4 token bằng `subtle.ConstantTimeCompare` rồi mới chọn actor, kèm test "token hợp lệ nhưng sai actor ⇒ 403".
- Đừng để `actor` trong body trở thành nguồn chân lý (actor spoof) — TR-23.

## Todo
- [ ] `ValidateRegistry()` + 4 env token, fail-closed từng biến
- [ ] `RegistryActorMiddleware` so sánh hằng-thời cả 4 token
- [ ] Handlers read + mutation (cursor theo version, body không có `actor`)
- [ ] Routes + openapi + redocly lint sạch

## Next Steps
Phase 6 chốt contracts/observability/runbook; Phase 7 gom evidence.
