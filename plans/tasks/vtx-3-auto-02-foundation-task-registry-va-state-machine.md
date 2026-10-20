# Phân tích task VTX-3 — Task Registry & State Machine (AUTO-02, Foundation)

> Generated: 2026-10-19 | Input: https://vtxsolution.atlassian.net/browse/VTX-3 | Trạng thái ticket: To Do
> Nguồn: Jira MCP (`jira_get_issue` ×2, `jira_get_worklog`); repo `vtx` tại HEAD `85a7d9a`: `apps/backend/db/migrations/00001..00003`, `apps/backend/db/queries/tasks.sql` + `jira.sql`, `apps/backend/internal/application/outbox_events.go`, `contracts/events/v1/events.json`, `apps/backend/go.mod`, `Makefile`, `.github/workflows/ci.yml`. Không dùng code intelligence đồ thị (GitNexus không có trong phiên này) — phạm vi code xác minh bằng đọc trực tiếp + grep văn bản; các phần chưa grep hết được ghi rõ.

## 1. Tóm tắt

VTX-3 là tầng "nguồn sự thật" của pipeline AI Automation (Epic VTX-1): sở hữu task identity, các `task_run`, state machine chuyển trạng thái hợp lệ, audit append-only và transactional outbox. Nó tiêu thụ event `execution_intent.created.v1` do VTX-2 (Done) đã persist trong `outbox_events` với ngữ nghĩa at-least-once, và phải tạo **đúng một** run cho mỗi intent. Điểm vào duy nhất để thay đổi trạng thái là `TransitionRun(ctx, runID, expectedVersion, expectedLeaseEpoch, action, actor, metadata)` với CAS version + lease-epoch fencing trong cùng một transaction. Ticket hiện ở To Do, **chưa có code VTX-3 nào trong repo** (xác minh: không có bảng `task_runs`/`task_run_events`/`deferred_intents`/`inbox_consumptions`, không có dispatcher outbox). Kết quả mong muốn: migration tương thích VTX-2, dispatcher outbox duy nhất, registry service Go, contracts/observability/runbook và TR-01..TR-25 có evidence tại exact SHA.

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
| Epic / Parent | VTX-1 ([AI Automation] Autonomous Development Pipeline — FMS.Application), status To Do |
| Created / Updated | 2026-10-13 17:08:04 +07 / 2026-10-16 10:21:06 +07 |
| Due date | không có dữ liệu |
| Thời gian đã log | không có dữ liệu (worklog trả về rỗng) |

## 3. Mô tả yêu cầu

Description là **SPEC-AUTO-02 v1.1** (v1.1 thay thế các diễn giải mơ hồ của v1.0), kèm 2 comment của reporter: #10017 phê duyệt v1.0 và #10018 phê duyệt v1.1 (giữ nguyên phạm vi và AC-01..12/TR-01..18, thêm phần quyết định bắt buộc §0 và TR-19..TR-25). Tóm lược các phần normative:

- **§0.A Trước khi viết migration:** phải khảo sát migration/sqlc/outbox payload/status field thực tế của VTX-2 tại exact target SHA, nộp **compatibility matrix** (bảng/cột/kiểu hiện tại → dùng gì/migration nào), giữ nguyên toàn bộ ID, row, unique constraint và outbox message đang chờ; migration phải additive và restart-safe, validate trên DB VTX-2 có dữ liệu. Không được đoán tên cột.
- **§0.B Invariants:** I-01 identity task `UNIQUE(jira_site_id, jira_issue_id)`, issue key chỉ là metadata đổi được; I-02 `UNIQUE(task_runs.intent_id)`, `UNIQUE(inbox_consumptions.consumer_name, source_event_id)`, `UNIQUE(task_run_events.run_id, version)`; I-03 tối đa **một run admitted nonterminal** cho mỗi task (partial unique index trên `task_id`, hoặc ràng buộc tương đương được test qua 2 DB client); I-04 intent hợp lệ đến khi đang có run nonterminal phải vào bảng `deferred_intents` bền vững (unique `intent_id`, có `received_at`, `disposition`, `reason`), FIFO theo `received_at` rồi `intent_id`, `pending_deferred` là **disposition của admission, không phải run state**; I-05 run mới `state=queued`, `version=0`, audit creation `version=0`, mỗi transition thành công tăng version đúng 1 và ghi 1 `task_run_events` + 1 outbox event; I-06 `completed/failed/cancelled` terminal, `blocked/retry_wait` vẫn nonterminal; I-07 mọi insert/update (task, run/deferred intent, receipt, audit, outbox) trong **một** DB transaction.
- **§0.C Bảng transition hợp lệ (machine values):** `queued --begin--> preparing` (scheduler VTX-5 + admission + lease hợp lệ); `preparing --advance--> coding`; `coding --advance--> verifying`; `verifying --advance--> qa`; `qa --advance--> quality_gate`; `quality_gate --advance--> pr_draft`; `pr_draft --complete--> completed` (orchestrator được uỷ quyền, Draft PR đã publish và policy hoàn tất — không phải merge/deploy); mọi stage nonterminal trừ `blocked/retry_wait` `--block--> blocked` (lưu `resume_state` + reason); `blocked --resume--> resume_state`; stage nonterminal trừ `blocked/retry_wait` `--schedule_retry--> retry_wait` (lưu `resume_state`, `retry_due_at`, budget bounded); `retry_wait --retry_due--> resume_state`; mọi nonterminal `--cancel--> cancelled`; mọi nonterminal `--fail--> failed`. Mọi transition khác bị cấm, kể cả self-transition và outgoing từ terminal. Các stage tương lai là **định nghĩa contract**, không phải bằng chứng hệ thống downstream đang chạy.
- **§0.D Lỗi ưu tiên:** `stale_version`/409 khi version lệch, `stale_lease`/409 khi lease cũ/hết hạn, `invalid_transition`/409 hoặc `forbidden`/403, `already_terminal`/409, `not_found`/404. So sánh version và (nếu có lease active) epoch/ownership/expiry **trong cùng transaction**. Lệnh operator trên `queued/blocked` trước khi có lease được phép bỏ epoch theo policy tường minh; **không có bypass epoch=0 vĩnh viễn**.
- **§0.E Outbox:** tái sử dụng dispatcher VTX-2 nếu có, nếu không thì VTX-3 implement dispatcher duy nhất cho `execution_intent.created.v1`; không tạo consumer cạnh tranh cùng ack một event; claim bounded `SKIP LOCKED` với owner/lease timeout, attempt count, `next_attempt_at` bền vững; **không giữ DB lock khi gọi mạng**; ACK nguồn chỉ sau commit thành công; tuyên bố "at-least-once delivery, exactly-once persisted effects". Retry backoff + jitter bounded; schema/version không hỗ trợ và poison event bị quarantine vào DLQ có thể kiểm tra kèm redrive; event do registry phát không được route ngược vào chính consumer của registry; phải tài liệu hoá backfill an toàn cho intent VTX-2 tồn tại trước đó.
- **§0.F/G:** VTX-3 sở hữu deferred enqueue/query/dedup/FIFO, VTX-5 sở hữu promotion + lease; tới khi VTX-5 có thì deferred intent **không được tự kích hoạt**. Log có cấu trúc (correlation/intent/run/event ID, error category đã sanitize, outbox lag, retry, DLQ), không log token hay raw webhook body; read API phân trang bằng cursor ổn định; mutation phải có caller được uỷ quyền. Bổ sung TR-19..TR-25 và DoD: TR-01..TR-25 + CI xanh tại exact SHA + chứng minh tương thích migration; **không được claim đã chạy coding/QA/gate/Draft PR**; test bắt buộc thiếu phải ghi **BLOCKED**, không PASS.
- **§1 Boundary:** không implement scheduler resource admission, Git worktree, coding agent, QA execution, GitHub PR creation hay Temporal workflow execution (thuộc VTX-4..VTX-13); tích hợp qua interface/contract ổn định.
- **§2 Tích hợp VTX-2:** extend (không tạo lại/đổi tên) `execution_intents`, `outbox_events`, inbox webhook, receipts; import đủ `intent_id`, `event_id`, `jira_site_id`, `issue_id`, `issue_key`, `changelog_id`, `correlation_id` + metadata UTC; field thiếu phải là lỗi schema/version chứ không được sinh giá trị thay thế; không hardcode "Ready to Dev"/status 10007 vì VTX-2 dùng `start_status_ids` cấu hình theo project.
- **§3 Data model:** `tasks` (unique `(jira_site_id, jira_issue_id)`, `jira_issue_key` mới nhất, project key, timestamp UTC, row version), `task_runs` (UUID `run_id`, FK task, unique FK `intent_id`, `correlation_id`, webhook event ID + changelog ID gốc, `state`, `version BIGINT`, `attempt`, timestamp, `commit_sha` nullable, `branch`, error code/message sanitized, `lease_owner`, `lease_epoch`, `lease_expires_at`), `task_run_events` append-only (unique `(run_id, version)`), receipt unique `(consumer_name, source_event_id)`, outbox event type versioned `task_run.created.v1`/`task_run.state_changed.v1`.
- **§4-§7:** `TransitionRun` là entrypoint mutation duy nhất, trả typed errors; CAS `WHERE run_id=? AND version=?`, không last-write-wins; một nonterminal active run mỗi Jira issue; port Go cho `RegistryRepository`, `EventPublisher/OutboxConsumer`, `Clock`, `LeaseValidator`; không endpoint đổi trạng thái ẩn danh; metrics runs created/transition counts/active runs/outbox lag/retry/DLQ/duplicate receipt; tất cả event và API có version, ordering theo run version.
- **§8-§11:** AC-01..AC-12 (migration không mất dữ liệu + sqlc tái lập được; 1 intent → 1 run `queued`; replay 3 lần không nhân bản; 20 writer đồng thời từ ≥2 instance → đúng một commit; restart an toàn; epoch cũ bị từ chối; re-entry hợp lệ không bị dedup sai; DLQ/backfill/redrive quan sát được; auth + audit phân trang; resume/cancel tường minh), TR-01..TR-25, DoD tại §10, và danh sách exclusion/handoff VTX-4..VTX-13.

## 4. Phân tích

### 4.0 Hiện trạng repo đã xác minh (đầu vào bắt buộc cho §0.A)

Migration hiện có: `00001_init_schema.sql` (bootstrap), `00002_jira_intake_and_outbox.sql` (VTX-2), `00003_webhook_receipts.sql` (VTX-2 review P2-1). Số migration kế tiếp là `00004`.

| Bảng/cột hiện tại | Kiểu | Dùng cho VTX-3 |
|---|---|---|
| `tasks(id VARCHAR(64) PK, title, status DEFAULT 'pending', prompt NOT NULL, workspace_path, created_at, updated_at)` | bootstrap | **Trùng tên với `tasks` của spec §3** nhưng khoá là `id VARCHAR(64)`, không có Jira identity. Chỉ được tham chiếu bởi code sqlc sinh ra (`persistence/db/tasks.sql.go`, `querier.go`); grep `CreateTask/ListTasks/task_events/agent_sessions` không thấy service nào dùng → hiện là schema chết nhưng có FK từ `task_events` và `agent_sessions` |
| `task_events(id BIGSERIAL, task_id → tasks(id) ON DELETE CASCADE, seq, event_type, payload TEXT, created_at)` | bootstrap | Trùng vai trò audit với `task_run_events` của spec; giữ nguyên hay để chết cần quyết định |
| `agent_sessions(task_id → tasks(id) ON DELETE CASCADE, ...)` | bootstrap | FK phụ thuộc khoá `tasks`; đổi identity của `tasks` sẽ ảnh hưởng |
| `execution_intents(intent_id UUID PK, source, issue_key, target_status_id, status CHECK pending/processing/done/failed, created_at, updated_at)` | VTX-2 | **Thiếu `jira_site_id`, `issue_id`, `changelog_id`, `correlation_id`** — identity thật nằm ở `jira_webhook_inbox` và payload outbox; không có unique constraint theo transition key |
| `jira_webhook_inbox(event_id UUID PK, jira_site_id NOT NULL, issue_id NOT NULL, issue_key, changelog_id, target_status_id, outcome CHECK, payload_hash, request_id, correlation_id, intent_id → execution_intents, error_detail, created_at, updated_at)` + `UNIQUE(jira_site_id, issue_id, changelog_id, target_status_id)` | VTX-2 | Nguồn `issue_id`/`jira_site_id` bền vững; dedup thật của VTX-2 nằm ở đây |
| `outbox_events(outbox_id UUID PK, intent_id UUID NOT NULL → execution_intents ON DELETE CASCADE, event_type VARCHAR(64), payload JSONB, published_at, created_at)` + partial index `(created_at) WHERE published_at IS NULL` | VTX-2 | **Thiếu toàn bộ cột claim/attempt/next_attempt_at/owner/lease/dead_letter/**`schema_version`** mà §0.E yêu cầu; FK `ON DELETE CASCADE` từ intent là rủi ro cho outbox đang chờ |
| `webhook_receipts(receipt_id, received_at, outcome CHECK 7 giá trị, reason_code, request_id, payload_hash, issue_key, error_detail, created_at)` | VTX-2 | Audit request phía webhook; **khác** `inbox_consumptions` mà spec yêu cầu (`consumer_name` + `source_event_id`) — không thay thế được |
| `contracts/events/v1/events.json` | contract | `PlatformEvent.type` enum mới có `execution_intent.created.v1`; **chưa có** `task_run.created.v1`/`task_run.state_changed.v1` |
| Payload `execution_intent.created.v1` (`internal/application/outbox_events.go`) | Go/JSON | Có đủ `event_id, intent_id, issue_id, issue_key, jira_site_id, changelog_id, target_status_id, occurred_at, occurred_at_source, correlation_id` — khớp danh sách field §2 phải import |
| Dispatcher outbox | — | **Không tồn tại**: chỉ có `InsertOutboxEvent` trong `db/queries/jira.sql` + sqlc output; không có code claim/publish/`published_at` update ngoài test. ⇒ VTX-3 phải implement designated dispatcher (§0.E) |
| Hạ tầng test/CI | Go 1.26.0, `github.com/vtx/platform`, chi v5, pgx v5.11, testify, Temporal SDK (đang có sẵn) | CI đã có job `backend-integration` (goose pinned v3.27.1, sqlc pinned): `goose up` → `down-to 0` → `up`, sqlc drift check, harness 20 concurrent × 2 replica qua nginx thật, upload evidence theo `${{ github.sha }}`; `make vet/test-backend/build-backend` chạy trong `backend-ci` |

### 4.1 Yêu cầu rõ ràng
- Phạm vi và boundary rõ: chỉ registry/state machine/outbox consumption; loại trừ tường minh scheduler, worktree, agent execution, QA, PR, Temporal (§1, §11).
- Contract state machine, action, actor và guard được liệt kê đầy đủ (§0.C), kèm quy tắc terminal/nonterminal (§0.B/I-06) và resume ngữ nghĩa `resume_state` (§0.C, AC-12).
- Đường mutation duy nhất `TransitionRun` với đúng bộ typed error và mã HTTP tương ứng (§0.D).
- Invariants dữ liệu cụ thể, kiểm chứng được bằng ràng buộc DB: I-01..I-03 nêu rõ unique/partial unique index, I-07 nêu rõ một transaction.
- Chính sách outbox rõ: một dispatcher designated, at-least-once + exactly-once persisted effects, ACK sau commit, không giữ lock khi I/O mạng (§0.E).
- DoD có bằng chứng cụ thể: TR-01..TR-25 + CI tại exact SHA + compatibility matrix migration; bắt buộc ghi BLOCKED thay vì PASS giả (§0.G, §10).
- Repo đã có sẵn hạ tầng đúng nhu cầu: goose/sqlc pinned, harness 2 replica 20 concurrent, evidence upload theo SHA → AC-05, TR-07, TR-11, TR-24 không phải xây mới từ đầu.

### 4.2 Yêu cầu mơ hồ / cần clarify
- **Xung đột tên `tasks`.** Spec §3 yêu cầu bảng `tasks` keyed by `(jira_site_id, jira_issue_id)`; repo đã có bảng `tasks(id VARCHAR(64), title, prompt, workspace_path, status)` từ bootstrap kèm `task_events`/`agent_sessions` FK vào `id`. Spec §2 chỉ nói extend `execution_intents`/`outbox_events`/inbox/receipts — không nói gì về bảng `tasks` sẵn có. Cần PO chốt: mở rộng additive bảng cũ (thêm cột nullable + partial unique, giữ FK cũ) hay tạo bảng mới dưới tên khác (vi phạm literal `tasks` của spec). Đây là quyết định migration không thể tự suy.
- **Identity cho `execution_intents`.** Payload outbox có `issue_id`/`jira_site_id`/`changelog_id`, nhưng bảng `execution_intents` không có các cột này. Chốt: chỉ đọc payload (không backfill cột) hay thêm cột additive + backfill từ `jira_webhook_inbox` để backfill/query độc lập với payload.
- **`task_events` bootstrap.** Giữ song song với `task_run_events` hay loại bỏ? Spec §3 nói "Prevent direct mutation outside transition service" nhưng không nói về bảng cũ. Hai bảng audit song song là nguồn nhầm lẫn dài hạn.
- **Phát hiện "unsupported schema/version".** Contract hiện chỉ biểu diễn version qua hậu tố `event_type` (`.v1`) và không có field version trong payload; chưa rõ VTX-3 chấp nhận/từ chối theo `event_type` suffix hay cần thêm `schema_version` vào payload (sửa contract VTX-2 đã Done ⇒ cần phê duyệt).
- **Actor/auth cho mutation.** Spec §7 chỉ nói "any mutating HTTP endpoint must be authenticated/authorized" và AC-11 nói actor/authorization được enforce; chưa có cơ chế cụ thể (JWT/OIDC/CLI/service token) hay danh sách actor (`scheduler`, `orchestrator`, `operator`, `policy`) map vào định danh nào.
- **Backfill (`document safe initial backfill`).** Chưa rõ môi trường nào có sẵn intent VTX-2 cần backfill, checkpoint lưu ở đâu, và backfill chạy như job một lần hay CLI.
- **MVP scope của transition.** §4 nói "Initial MVP registry may allow only `queued`, `blocked`, `cancelled` transitions operationally" trong khi §0.C liệt kê toàn bộ bảng là normative. Cần chốt là các stage sau `queued` sẽ được implement đầy đủ (chỉ không gọi hệ thống thật) hay chỉ định nghĩa contract/test.

### 4.3 Phạm vi ảnh hưởng (xác minh bằng đọc trực tiếp, chưa dùng code intelligence)

Đã kiểm tra: `db/migrations/00001..00003`, `db/queries/tasks.sql`, `db/queries/jira.sql`, `internal/application/outbox_events.go`, `internal/application/jira_webhook_service.go` (chỉ phần liên quan outbox), `contracts/events/v1/events.json`, `apps/backend/go.mod`, `Makefile`, `.github/workflows/ci.yml`.

- **Schema mới bắt buộc (migration `00004+`):** `task_runs`, `task_run_events`, `deferred_intents`, `inbox_consumptions`; và các cột claim/lease/DLQ cho `outbox_events`.
- **Schema phải extend additive, không phá dữ liệu:** `outbox_events` (claim owner/lease timeout, `attempt`, `next_attempt_at`, trạng thái dead-letter + `reason`, `schema_version`), có thể `execution_intents` (identity columns) tuỳ quyết định §4.2.
- **Bảng có nguy cơ xung đột tên/vai trò:** `tasks`, `task_events`, `agent_sessions` (bootstrap, xem §4.2).
- **sqlc:** `apps/backend/db/queries/` (thêm query registry + claim outbox) và regenerate `internal/persistence/db/*.go`; CI có bước drift check nên không thể sửa tay.
- **Contracts:** `contracts/events/v1/events.json` cần thêm hai event type + definition; `contracts/openapi/v1/openapi.yaml` nếu thêm endpoint read/mutation (chưa xác minh nội dung file này trong phiên).
- **Go code mới:** package registry (domain/service/repository), outbox dispatcher + DLQ, port `Clock`/`LeaseValidator`, CLI/HTTP auth wrapper, metrics/log; `cmd/worker`/`cmd/api` wiring (chưa đọc chi tiết `cmd/*`).
- **Chưa kiểm tra:** toàn văn `internal/orchestration/*` và `internal/agent/*` (thuộc VTX-4..VTX-7 — cần biết chúng có đọc/ghi `tasks`/`task_events` hay không), `infra/compose/*` cho integration, `internal/config/*`. Nếu `orchestration`/`agent` hiện dùng bảng bootstrap thì quyết định §4.2 có thêm ràng buộc.

### 4.4 Rủi ro / cạm bẫy
- **Tên bảng `tasks` đã bị chiếm bởi schema bootstrap khác hình dạng** — migration "tạo bảng `tasks`" theo spec sẽ fail hoặc (tệ hơn) buộc phải `DROP`/rename bảng có FK từ `task_events`/`agent_sessions`. Đây là rủi ro migration cao nhất của ticket.
- **`outbox_events.intent_id` là `NOT NULL ... ON DELETE CASCADE`** từ VTX-2: mọi event do registry phát (kể cả `task_run.state_changed.v1`) phải gắn được `intent_id`; xoá intent sẽ xoá luôn outbox đang chờ, mâu thuẫn với yêu cầu "preserve outstanding outbox messages" nếu có thao tác dọn dẹp intent.
- **Chưa có dispatcher nào để tái sử dụng** — §0.E cho phép VTX-3 tự làm, nhưng nghĩa là ticket này gánh thêm claim/lease/DLQ/backfill, tăng bề mặt test (TR-12, TR-13, TR-22).
- **Không được giữ DB lock khi gọi mạng** (§0.E/§5) trong khi AC-05/TR-07 yêu cầu 20 writer đồng thời từ ≥2 instance: cấu hình test phải là nhiều tiến trình thật, không phải goroutine trong một process, nếu không evidence sẽ không đủ.
- **Backfill intent tồn tại trước đó** có thể tạo run trùng hoặc bỏ sót nếu checkpoint không bền vững; spec yêu cầu tài liệu hoá nhưng chưa định nghĩa checkpoint — rủi ro im lặng mất re-entry.
- **`commit_sha` nullable** là yêu cầu tường minh (§3, AC-02): dễ bị "siết" thành NOT NULL khi implement nếu review đọc sai.
- **Ranh giới outbox recursion:** nếu routing theo `consumer_name`/`event_type` không chặn đúng, registry có thể tự tiêu thụ event do mình phát (§6/§0.E).
- **Audit trùng vai trò** giữa `task_events` (bootstrap) và `task_run_events` (spec) tạo nguy cơ mutate ngoài transition service, đúng thứ mà §3 cấm.
- **Temporal SDK đã nằm trong `go.mod`** (kèm `internal/orchestration`): rủi ro implement vượt boundary, vi phạm §1/TR-16, nếu không có test chặn side-effect.
- **Yêu cầu evidence tại exact SHA** với 25 test case: nếu thiếu môi trường (DB có dữ liệu VTX-2, 2 replica), nhiều TR sẽ phải ghi BLOCKED theo đúng luật của ticket — cần chốt sớm cách tạo môi trường đó.

### 4.5 Phụ thuộc
- Blocked by: VTX-2 (AUTO-01 Jira Event Intake & Deduplication — Done) theo issue link "Blocks"; trong description: VTX-2 (Done) và VTX-14 (Bootstrap).
- Blocks: VTX-4 (Git Worktree Lifecycle & Lease), VTX-5 (Scheduler & Resource Limits), VTX-7 (Isolated Coding Agent Runner).
- Epic liên quan: VTX-1 (parent, To Do).

## 5. Câu hỏi mở

1. Xử lý xung đột bảng `tasks`: mở rộng additive bảng bootstrap (thêm Jira identity columns + partial unique, giữ FK `task_events`/`agent_sessions`) hay tạo bảng mới? Hệ quả tương ứng với `task_events`/`agent_sessions` là gì?
2. `execution_intents` có được thêm cột `jira_site_id`/`issue_id`/`changelog_id` (additive + backfill từ `jira_webhook_inbox`) hay VTX-3 chỉ đọc các field này từ payload outbox?
3. Cơ chế xác định "unsupported schema/version": dùng hậu tố `event_type` hiện có, hay bổ sung `schema_version` vào payload (cần sửa contract VTX-2 đang ở trạng thái Done)?
4. Actor/auth cho mutation: dùng cơ chế nào (OIDC, service token, JWT, CLI) và map các actor `scheduler`/`orchestrator`/`operator`/`policy` vào định danh ra sao?
5. Backfill intent VTX-2 tồn tại trước: môi trường nào có dữ liệu thật, checkpoint lưu ở đâu, chạy dạng job một lần hay CLI?
6. MVP lần này implement đầy đủ bảng transition của §0.C (không gọi hệ thống downstream), hay chỉ `queued`/`blocked`/`cancelled` như gợi ý ở §4?
7. `policy` hoàn tất cho `pr_draft --complete--> completed` trong Phase 1 là gì (điều kiện nào được coi là "completion policy met")?

## 6. Đề xuất bước tiếp theo

- Chốt §5 câu 1, 2, 3 với PO trước khi viết migration — đây là các quyết định làm thay đổi hình dạng schema và contract của hệ thống đã Done.
- Thực hiện §0.A: tạo compatibility matrix từ `00001..00003` + payload contract (bảng ở §4.0 là bản nháp đã có bằng chứng), chạy migration trên DB có dữ liệu VTX-2 và lưu evidence.
- Viết test tái lập trước cho các invariant ràng buộc được ở tầng DB (I-01, I-02, I-03 partial unique qua 2 client; I-05 version=0) — nếu ràng buộc không biểu diễn được bằng SQL thì phải chứng minh bằng lock/transaction.
- Mở rộng hạ tầng integration hiện có (`backend-integration`: goose up/down/up, sqlc drift, harness 20 concurrent × 2 replica, evidence theo SHA) thay vì dựng mới; thêm bước tái sử dụng cho TR-07/TR-11/TR-18/TR-24.
- Chốt sớm cơ chế actor/auth và định dạng log/metric để TR-14/TR-23 có chỗ bám.
- Với các TR phụ thuộc môi trường (DB populated, 2 instance, DLQ redrive), xác nhận trước khả năng tạo môi trường; nếu không, ghi BLOCKED theo đúng yêu cầu của ticket thay vì PASS hình thức.

---
*Report chỉ phục vụ phân tích — không phải plan implement.*
