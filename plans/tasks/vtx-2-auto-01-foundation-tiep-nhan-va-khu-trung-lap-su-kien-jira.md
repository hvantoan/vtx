# Phân tích task VTX-2 — Tiếp nhận và khử trùng lặp sự kiện Jira (Foundation)

> Generated: 2026-10-15 | Input: https://vtxsolution.atlassian.net/browse/VTX-2 | Trạng thái ticket: To Do  
> Nguồn: Jira MCP (`jira_vtx_jira_get_issue`), comments chỉ đạo #10011 và #10012 của Hồ Văn Toàn; đối chiếu cấu trúc hiện tại của repo `vtx` (`apps/backend`, `contracts/openapi/v1/openapi.yaml`, `apps/backend/db/migrations/00001_init_schema.sql`, `AGENTS.md`)

## 1. Tóm tắt

Task VTX-2 thuộc Phase 1 (Foundation) của Epic VTX-1, nhằm xây dựng cơ chế tiếp nhận webhook từ Jira Cloud an toàn và có khả năng phục hồi khi một task chuyển trạng thái sang `Ready to Dev`. Trọng tâm kỹ thuật là đảm bảo tính duy nhất và bền vững (idempotency): mỗi chuyển đổi trạng thái hợp lệ chỉ sinh ra đúng một ý định thực thi (`execution_intent`) và một bản ghi sự kiện transactional outbox trong PostgreSQL; mọi lần phân phối lặp lại của cùng một chuyển đổi trạng thái đều bị loại trừ trùng lặp mà không tạo thêm intent. Ranh giới nhiệm vụ được giới hạn chặt chẽ ở tầng nạp dữ liệu HTTP, xác thực bảo mật, re-fetch kiểm chứng qua Jira API và giao dịch cơ sở dữ liệu; tuyệt đối không khởi chạy workflow Temporal, không cấp phát Git worktree hay chiếm dụng tài nguyên thực thi agent (thuộc phạm vi của VTX-3 và VTX-5).

## 2. Thông tin ticket

| Mục | Giá trị |
|---|---|
| Type | Task |
| Status | To Do |
| Priority | Medium |
| Assignee | Hồ Văn Toàn |
| Reporter | Hồ Văn Toàn |
| Components | không có dữ liệu |
| Labels | ai-automation, fms-application, phase-1 |
| Fix version | không có dữ liệu |
| Epic / Parent | VTX-1 ([AI Automation] Autonomous Development Pipeline — FMS.Application) |
| Created / Updated | 2026-10-13 17:08:02 +07 / 2026-10-15 00:37:57 +07 |
| Due date | không có dữ liệu |
| Thời gian đã log | không có dữ liệu |

## 3. Mô tả yêu cầu

*Đặc tả kỹ thuật được phê duyệt theo SPEC-AUTO-01 v1.0 và Quyết định kiến trúc Jira Free Webhook (Amendment 01, ngày 15/10/2026):*

### 3.1 Mục tiêu và Phạm vi
- Cung cấp HTTP endpoint tiếp nhận webhook Jira Cloud an toàn, có khả năng phục hồi khi issue chuyển sang trạng thái `Ready to Dev`.
- Một chuyển đổi hợp lệ và còn hiệu lực phải sinh ra **chính xác một durable execution intent** và **một bản ghi transactional outbox**; gửi lại cùng một chuyển đổi không sinh intent thứ hai.
- Không khởi chạy agent, không trigger Temporal workflow, không cấp phát worktree hay chiếm concurrency slot.

### 3.2 Cấu hình triển khai Webhook Jira Cloud Free (Amendment 01)
- **Mô hình tích hợp:** Jira Cloud Free Administrative Webhook (cấu hình bởi Jira Admin: sự kiện `Issue updated` kèm changelog, bộ lọc project `VTX`) → HTTPS Secure Webhook Gateway → Private Go Intake Service.
- **Xác thực đầu vào:** Không giả định Jira Cloud gửi header `Authorization` tùy ý hay chữ ký HMAC. Sử dụng một chuỗi bí mật ngẫu nhiên có độ dài/entropy cao đặt trên đường dẫn URL webhook (`URL path secret`). Gateway đảm nhiệm:
  - Chấm dứt TLS tại Gateway.
  - So khớp secret trên path bằng thuật toán so sánh thời gian hằng số (constant-time comparison).
  - Loại bỏ (redact) secret khỏi URL chuyển tiếp, access logs, traces, alerts.
  - Bóc tách toàn bộ header xác thực nội bộ do client giả mạo gửi lên và tiêm (inject) header/token xác thực nội bộ đáng tin cậy vào request gửi tới Go service.
- **Ranh giới mạng:** Mạng nội bộ riêng giữa Gateway và Go intake; tuyệt đối không mở bypass công khai trực tiếp tới Go service.
- **Kiểm chứng Tenant:** Khả năng quản trị Webhook trên gói Jira Free có thể khác nhau tùy tenant; bắt buộc kiểm tra menu Admin Webhooks và payload thực tế trên Jira site trước khi ghi nhận thành công. Nếu thiếu quyền/tính năng, phải báo cáo BLOCKED cùng bằng chứng, không âm thầm thay thế bằng endpoint không xác thực.

### 3.3 HTTP Ingress và An toàn dữ liệu
- **Route:** `POST /api/v1/integrations/jira/webhooks`, yêu cầu HTTPS tại ingress; phản hồi JSON kèm `request_id` và trạng thái xử lý (disposition), không làm lộ payload hoặc thông tin nhạy cảm.
- **Xác thực tại Go Service:** Nhận token xác thực qua header `Authorization: Bearer <secret>` do Gateway tiêm vào. Từ chối ngay lập tức (fail-closed) nếu thiếu hoặc sai token.
- **Bảo vệ Ingress:** Giới hạn kích thước payload tối đa 256 KiB, bắt buộc `Content-Type: application/json`, timeout request, kiểm tra schema, chỉ cho phép các project/event trong allowlist, áp dụng rate limiting.
- **Phòng chống SSRF & Quản lý Token:** Sử dụng Jira API Token với quyền tối thiểu (least-privilege); Jira Host được cấu hình cố định trong hệ thống, không cho phép URL do người dùng chỉ định.
- **Mã phản hồi HTTP chuẩn:**
  - `401`/`403`: Không xác thực / không có quyền.
  - `400`: Payload/schema không hợp lệ.
  - `413`: Payload vượt quá 256 KiB.
  - `422`: Thiếu changelog ID hoặc không tìm thấy chuyển đổi trạng thái hợp lệ.
  - `202`: Chấp nhận xử lý (kèm disposition: `accepted`, hoặc `ignored_stale`, `ignored_scope`).
  - `200`: Đã tiếp nhận trước đó (disposition: `duplicate`, trả kèm intent ID hiện có).
  - `503`: Lỗi tạm thời phía Jira API hoặc DB để Jira webhook thực hiện retry.

### 3.4 Định danh sự kiện và Cơ chế khử trùng lặp (Idempotency)
- **Định danh bất biến của chuyển đổi trạng thái (Transition Identity):**
  $$\text{Transition Key} = (\text{jira\_site\_id}, \text{issue\_id}, \text{changelog\_id}, \text{target\_status\_id})$$
- **Ràng buộc bắt buộc:** Trường `changelog.id` là bắt buộc. Nếu payload không có `changelog.id`, trả mã 422 và ghi log audit; tuyệt đối không băm (hash) dựa trên `issue_key + status` vì sẽ làm mất các lần quay lại trạng thái `Ready to Dev` hợp lệ sau này.
- **Lưu trữ bảng `jira_webhook_inbox`:** Lưu event key, issue ID/key, changelog ID, timestamps, disposition outcome, correlation/request ID, mã băm payload SHA-256 (không lưu plaintext nhạy cảm), metadata retry/error và khóa ngoại tới intent.
- **Tính duy nhất nguyên tử:** Được bảo đảm bằng PostgreSQL Unique Index trên composite key, không dựa vào mutex trong bộ nhớ ứng dụng hay cơ chế `SELECT` trước `INSERT`.
- **Phân biệt ngữ nghĩa:** Cùng một transition gửi 3 lần chỉ tạo 1 intent. Nhưng nếu issue sau đó rời `Ready to Dev` rồi quay lại `Ready to Dev` với một `changelog.id` mới, hệ thống phải ghi nhận đó là một intent mới.

### 3.5 Quy trình xử lý và Giao dịch nguyên tử (Durable Transaction)
1. Xác thực transport và envelope của webhook; lọc sự kiện chuyển trạng thái sang `Ready to Dev`.
2. Chuẩn hóa issue ID, site ID, project, transition ID, request ID.
3. **Re-fetch Jira API:** Gọi Jira REST API với context deadline và cơ chế retry có giãn cách (tuân thủ header `Retry-After` khi gặp 429) để kiểm tra issue ID/project và xác nhận trạng thái hiện tại (current status) thực sự vẫn là `Ready to Dev`.
4. Nếu trạng thái trên Jira không còn là `Ready to Dev` hoặc project nằm ngoài allowlist: lưu bản ghi inbox với disposition `ignored_stale` hoặc `ignored_scope`, không tạo intent.
5. **Đơn giao dịch PostgreSQL (Single DB Transaction):**
   - Insert vào `jira_webhook_inbox` theo event key.
   - Insert vào `execution_intents` với trạng thái `pending`.
   - Insert vào `outbox_events` với loại `execution_intent.created.v1` tham chiếu tới intent.
   - Cập nhật outcome của inbox thành `accepted`.
   - Nếu xảy ra xung đột unique (duplicate delivery): truy vấn lại disposition và intent ID hiện có để trả về HTTP 200 Duplicate.
6. Commit transaction thành công trước khi phản hồi HTTP 202. Không gọi Temporal, Git, agent hay Docker trong transaction HTTP.
7. Nếu bước đọc Jira hoặc commit DB thất bại, trả về lỗi retryable (HTTP 503), bảo đảm không có intent/outbox bị lưu dở dang (rollback hoàn toàn).

### 3.6 Tiêu chí nghiệm thu (Acceptance Criteria)
- **AC-01:** Chuyển đổi trạng thái sang `Ready to Dev` hợp lệ, đúng scope tạo ra đúng 1 pending intent và 1 outbox event trong cùng một DB transaction.
- **AC-02:** Replay cùng một sự kiện $\ge 3$ lần hoặc 20 request đồng thời qua ít nhất 2 instances API chỉ tạo ra đúng 1 intent/outbox, các request trùng lặp trả về cùng intent identifier.
- **AC-03:** Webhook giả mạo/không xác thực bị từ chối; không fetch Jira, không tạo DB intent; secret không bao giờ xuất hiện trong log; payload quá cỡ hoặc sai format bị từ chối.
- **AC-04:** Re-fetch Jira xác thực đúng trạng thái hiện tại; transition cũ/sai lệch không tạo intent và có vết disposition rõ ràng.
- **AC-05:** Changelog ID khác biệt khi issue re-entry vào `Ready to Dev` lần sau không bị nhầm là duplicate.
- **AC-06:** Lỗi Jira 429/5xx/timeout hoặc DB rollback dẫn đến kết quả retryable và không để lại intent/outbox nửa vời.
- **AC-07:** Migration SQL (up/down/re-up) và sinh mã sqlc tái lập sạch sẽ từ DB mới; ràng buộc unique và index được kiểm chứng.
- **AC-08:** Hợp đồng versioned OpenAPI/event schema và tài liệu vận hành runbook giải thích rõ auth, đăng ký webhook, allowlist project/status ID, cơ chế retry & replay.
- **AC-09:** Bộ test unit & integration, audit/metrics, kiểm tra CI Go vet/test/race và migration/concurrency integration đều PASS.
- **AC-10:** Bàn giao sang VTX-3/VTX-5 mô tả rõ quy tắc outbox consumer, scheduler revalidation và re-entry; không thực thi tác vụ agent trong VTX-2.

### 3.7 Ma trận kiểm thử (JT-01 đến JT-18)
- `JT-01`: Transition hợp lệ → HTTP 202, 1 inbox, 1 intent, 1 outbox.
- `JT-02`: Gửi lại cùng event 3 lần → lần đầu accepted, các lần sau duplicate; 1 intent duy nhất.
- `JT-03`: 20 request đồng thời qua 2 instances → chính xác 1 intent/outbox.
- `JT-04`: Sai secret, thiếu auth, sai content-type, JSON lỗi, dung lượng > 256 KiB → bị từ chối.
- `JT-05`: Không có changelog ID hoặc không phải status transition → trả lỗi không chấp nhận (422), không tạo intent.
- `JT-06`: Trạng thái Jira thay đổi trước khi re-fetch → ghi nhận `ignored_stale`, không tạo intent.
- `JT-07`: Project/Site không thuộc allowlist → ghi nhận `ignored_scope`, không tạo intent.
- `JT-08`: Jira API gặp 429/500/timeout → trả kết quả retryable, không tạo intent; retry sau đó thành công.
- `JT-09`: Lỗi DB trong quá trình ghi → rollback toàn bộ transaction, retry sau đó thành công.
- `JT-10`: Re-entry sau đó vào `Ready to Dev` với changelog ID mới → tạo intent mới.
- `JT-11`: Tuân thủ chuẩn OpenAPI và schema outbox event; không để lộ secret trong log/event.
- `JT-12`: Audit metrics và correlation ID hoạt động đúng; khởi động lại & replay bảo toàn tính idempotent.
- `JT-13`: Kiểm tra migration SQL up/down/re-up và tính toàn vẹn của unique constraint dưới tải đồng thời.
- `JT-14`: Handler không bao giờ gọi Temporal/agent/Docker; ranh giới bàn giao consumer dừng ở persistence.
- `JT-15`: Request gửi qua URL path secret tại Gateway được forward thành công với header xác thực nội bộ.
- `JT-16`: URL path secret sai/thiếu hoặc bypass trực tiếp tới Go intake bị từ chối.
- `JT-17`: Gateway bóc tách header auth giả mạo và che giấu (redact) URL secret trong toàn bộ logs.
- `JT-18`: Cơ chế xoay vòng secret (secret rotation) với cửa sổ hiệu lực cũ/mới và từ chối sau khi hết hạn.

## 4. Phân tích

### 4.1 Yêu cầu rõ ràng
1. **Ranh giới công nghệ:**
   - Sử dụng Go, router `go-chi/chi/v5`, thư viện kết nối PostgreSQL `pgx/v5`, sinh mã truy vấn `sqlc`.
   - Logging cấu trúc bằng chuẩn `log/slog` (không dùng external logger), không in plaintext payload hay secrets ra log.
2. **Cơ chế xác thực Ingress:**
   - Mô hình 2 lớp: Secure Gateway xác thực URL path secret → chuyển tiếp vào Go intake kèm header `Authorization: Bearer <internal_token>`.
   - Go intake chỉ tin cậy internal token, kiểm tra bằng constant-time comparison, từ chối mọi truy cập trực tiếp từ bên ngoài không qua gateway.
3. **Tính toàn vẹn của nghiệp vụ:**
   - Webhook chỉ đóng vai trò tín hiệu kích hoạt (trigger signal). Jira REST API mới là nguồn chân lý (source of truth) cho trạng thái của issue.
   - Bắt buộc kiểm tra `changelog.id` để phân biệt từng sự kiện chuyển đổi trạng thái.
   - Đảm bảo tính nguyên tử của 3 thực thể trong 1 transaction DB duy nhất: `jira_webhook_inbox` + `execution_intents` + `outbox_events`.
4. **Không vượt phạm vi (No Scope Creep):**
   - VTX-2 chỉ lưu `outbox_events`, không viết consumer đọc outbox để đẩy sang Temporal hay worker (VTX-3 sẽ đảm nhiệm Outbox Publisher/Relay).

### 4.2 Yêu cầu mơ hồ / Cần clarify
1. **Status ID thực tế của "Ready to Dev" trên Jira:**
   - Jira workflow có thể lưu tên hiển thị là "Ready to Dev", nhưng backend phải cấu hình theo numeric `status ID` (ví dụ `10001` hoặc `10012`) để tránh sai lệch khi đổi ngôn ngữ hiển thị. Cần truy vấn Jira API để lấy chính xác ID này trên tenant `vtxsolution.atlassian.net`.
2. **Cấu hình Secure Webhook Gateway cho môi trường Dev & Local:**
   - Cần chốt thành phần gateway cụ thể trong `docker-compose.yml` (ví dụ: Nginx proxy hoặc Traefik/Caddy cấu hình URL rewrite và inject header) để phục vụ việc kiểm thử JT-15..JT-18 trên local/CI.
3. **Allowlist Project Keys và Issue Types:**
   - Hiện tại cấu hình mặc định là project `VTX`. Cần xác định các loại issue hợp lệ: Task, Story, Bug hay tất cả?

### 4.3 Phạm vi ảnh hưởng (Codebase & Kiến trúc)
- **Hợp đồng giao tiếp (`contracts/openapi/v1/openapi.yaml`):**
  - Bổ sung định nghĩa path `/api/v1/integrations/jira/webhooks` (phương thức `POST`).
  - Định nghĩa request body (Jira Webhook Envelope), response models (`202 Accepted`, `200 Duplicate`, `400`, `401`, `422`, `503`).
- **Cơ sở dữ liệu & Migration (`apps/backend/db/`):**
  - Tạo migration mới `00002_jira_intake_and_outbox.sql` chứa định nghĩa 3 bảng:
    - `jira_webhook_inbox` (composite unique index trên `jira_site_id`, `issue_id`, `changelog_id`, `target_status_id`).
    - `execution_intents` (lưu intent trạng thái `pending`, thông tin issue, project).
    - `outbox_events` (chứa payload `execution_intent.created.v1`, trạng thái pending/published).
  - Viết các query trong `apps/backend/db/queries/jira_webhooks.sql` và chạy `sqlc generate`.
- **Backend Go Modules (`apps/backend/internal/`):**
  - `internal/integrations/jira/`: Client gọi Jira REST API (re-fetch issue, xử lý backoff, kiểm tra status ID, parse changelog).
  - `internal/application/`: Service xử lý logic tiếp nhận, validate, điều phối transaction DB.
  - `internal/transport/http/`: Handler xử lý endpoint webhook, parse JSON, middleware kiểm tra internal auth header.
  - `internal/config/`: Bổ sung các biến môi trường cho Jira Base URL, Jira API Token, Inbound Gateway Secret, Allowed Projects, Ready to Dev Status ID.
- **Hạ tầng (`infra/compose/`):**
  - Cập nhật biến môi trường mẫu trong `.env.example`.
  - Bổ sung cấu hình reverse proxy gateway cho testing JT-15..18 nếu chạy tích hợp qua docker-compose.

### 4.4 Rủi ro / Cạm bẫy
1. **Lộ Secret qua Log hệ thống:**
   - Do secret nằm trên đường dẫn URL (`/webhooks/<secret>`), nếu reverse proxy hoặc access logger không cấu hình che giấu (redaction) đúng cách, token sẽ bị ghi vào log file, metric hoặc APM trace.
2. **Nghẽn / Rate Limit Jira API (HTTP 429):**
   - Nếu Jira bắn hàng loạt webhook cùng lúc, việc re-fetch đồng thời có thể khiến Jira API trả về `429 Too Many Requests`. Cần xử lý header `Retry-After` và trả về `503` để Jira webhook tự động retry sau đó.
3. **Race Condition khi nhiều Webhook Replicas nhận cùng 1 event:**
   - Việc kiểm tra `SELECT` tồn tại trước khi `INSERT` sẽ gây race condition giữa 2 request chạy đồng thời. Bắt buộc phải thực hiện `INSERT` và bắt lỗi vi phạm unique index từ PostgreSQL để đảm bảo tính nguyên tử.
4. **Hiện tượng Inherent State Race:**
   - Sau khi Go service re-fetch Jira thấy status là `Ready to Dev` và commit intent vào DB, nhưng trạng thái trên Jira lại bị đổi sang status khác ngay sau đó. Kiến trúc đã quy định rõ: VTX-3/VTX-5 khi nhấc outbox để lên lịch sẽ phải re-validate lần nữa trước khi chạy.

### 4.5 Phụ thuộc
- **Blocked by:** VTX-14 (Bootstrap Go + Next.js Monorepo — hiện đã hoàn thành bước nền tảng, code structure đã sẵn sàng).
- **Blocks:** VTX-3 (Outbox Publisher / Relay sang Temporal) và VTX-5 (Scheduler & Concurrency Manager).
- **Epic liên quan:** VTX-1 ([AI Automation] Autonomous Development Pipeline — FMS.Application).

## 5. Câu hỏi mở

1. **Status ID chính xác của trạng thái "Ready to Dev" trên site Jira `vtxsolution.atlassian.net` là bao nhiêu?** (Cần gọi Jira API hoặc kiểm tra workflow scheme trên Jira site).
2. **Quyền hạn tài khoản Jira Free Admin:** Cần xác nhận tài khoản Jira hiện tại có quyền truy cập vào `Jira Settings -> System -> WebHooks` trên tenant để cấu hình webhook endpoint hay không.
3. **Thành phần Gateway dự kiến sử dụng cho môi trường Staging/Production:** Sẽ dùng Nginx, Cloudflare Worker hay API Gateway của Cloud Provider để phục vụ việc cấu hình URL secret redaction?

## 6. Đề xuất bước tiếp theo

1. **Xác minh môi trường Jira Cloud:** Sử dụng token Jira hiện có để truy vấn endpoint `/rest/api/3/status` hoặc `/rest/api/3/issue/VTX-2` nhằm xác định chính xác numeric `status ID` của "Ready to Dev" và kiểm tra quyền cấu hình Admin Webhook trên tenant `vtxsolution.atlassian.net`.
2. **Định nghĩa OpenAPI & Migration Schema:**
   - Cập nhật `contracts/openapi/v1/openapi.yaml` cho route webhook.
   - Tạo migration `00002_jira_intake_and_outbox.sql` và sinh code `sqlc` cho persistence layer.
3. **Xây dựng Jira API Client & Test Fixtures:**
   - Viết client Go gọi Jira API có kiểm soát timeout, retry, backoff 429.
   - Chuẩn bị fixtures webhook payload giả lập cho test suite (JT-01 đến JT-14).
4. **Hiện thực hóa Gateway Stub & Test Matrix JT-15..18:**
   - Cấu hình gateway kiểm thử (stub gateway) để chạy tự động bộ test từ JT-01 đến JT-18 trước khi chuyển task sang In Review.

---
*Report chỉ phục vụ phân tích — không phải plan implement.*
