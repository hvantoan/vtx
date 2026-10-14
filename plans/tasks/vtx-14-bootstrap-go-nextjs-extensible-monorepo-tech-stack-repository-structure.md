# Phân tích task VTX-14 — Khởi tạo Monorepo Go + Next.js và cấu trúc Tech Stack nền tảng

> Generated: 2026-10-14 | Input: https://vtxsolution.atlassian.net/browse/VTX-14 | Trạng thái ticket: To Do  
> Nguồn: Jira MCP (`jira_vtx_jira_get_issue`), SPEC-001 v1.2 từ description của VTX-14, parent epic VTX-1; kiểm tra workspace cục bộ `/home/vif/workspace/vtx` (thư mục rỗng, chưa khởi tạo git hay source code)

## 1. Tóm tắt

Task VTX-14 đảm nhiệm việc khởi tạo nền tảng (bootstrap) cho một monorepo độc lập theo định hướng backend-first (Go + Next.js), phục vụ hệ thống tự động hóa lập trình AI (Autonomous Development Pipeline) với mục tiêu ứng dụng ban đầu là FMS.Application. Dự án thiết lập cấu trúc repo có khả năng mở rộng với ba phạm vi ứng dụng chính: `apps/backend` (Go module duy nhất), `apps/web` (Next.js) và `apps/agent-runtime` (Node.js host cho Pi/OMP agent), kết hợp hạ tầng Temporal orchestration và cơ sở dữ liệu PostgreSQL 17. Kết quả mong muốn là cung cấp bộ khung hoàn chỉnh bao gồm domain/application interfaces, workflow mẫu trên Temporal Go SDK, hợp đồng giao tiếp (OpenAPI, JSON-RPC IPC), kịch bản Docker Compose, các lệnh quản lý qua Makefile và pipeline CI cơ bản, làm tiền đề vững chắc cho các giai đoạn tiếp theo (chạy 3 task song song với worktree/Docker sandbox, coding agent và Quality Gate).

## 2. Thông tin ticket

| Mục | Giá trị |
|---|---|
| Type | Task |
| Status | To Do |
| Priority | High |
| Assignee | Hồ Văn Toàn |
| Reporter | Hồ Văn Toàn |
| Components | không có dữ liệu |
| Labels | ai-automation, architecture, bootstrap, fms-application, phase-0 |
| Fix version | không có dữ liệu |
| Epic / Parent | VTX-1 ([AI Automation] Autonomous Development Pipeline — FMS.Application) |
| Created / Updated | 2026-10-14 09:33:59 +07 / 2026-10-14 10:08:34 +07 |
| Due date | không có dữ liệu |
| Thời gian đã log | không có dữ liệu |

## 3. Mô tả yêu cầu

*Tóm lược theo đặc tả SPEC-001 v1.2 được phê duyệt trong Jira description:*

### 3.1 Mục tiêu và phạm vi
- Khởi tạo nền tảng độc lập, hỗ trợ tiếp nhận task từ Jira, điều phối qua Temporal, tiến tới chạy đồng thời 3 task với Git worktree/branch riêng, Docker sandbox, điều phối Pi/OMP coding agent, QA độc lập và Quality Gate xác định.
- Giai đoạn bootstrap này chỉ bàn giao: cấu trúc nền tảng, các interfaces/contracts, workflow mẫu, CI và triển khai local bằng Docker Compose; không bao gồm việc tự động hóa toàn bộ luồng code thật hay tự tạo Draft PR.

### 3.2 Quyết định công nghệ (Ràng buộc bắt buộc)
- **Core Backend:** Go (ghim phiên bản ổn định trong `go.mod` và CI). Sử dụng Go `net/http` tiêu chuẩn hoặc router nhẹ (ưu tiên `chi`) cho REST/SSE, context propagation, health/readiness probe, webhook validation và RBAC middleware.
- **Temporal:** Temporal Self-hosted + Temporal Go SDK. Mã workflow Go bắt buộc phải deterministic; toàn bộ network/DB/Git/Docker/agent I/O chỉ được phép thực hiện trong Activities.
- **Persistence:** PostgreSQL 17 + `pgx` và `sqlc` (tiếp cận SQL-first); migration SQL có phiên bản (`golang-migrate` hoặc `Goose`, chọn 1 công cụ trước khi triển khai). Tách biệt cơ sở dữ liệu lưu trữ của Temporal khỏi platform schema.
- **Web:** Next.js + React + TypeScript strict mode + Tailwind CSS + shadcn/ui. Web giao tiếp với Go API qua REST/SSE; không nắm giữ secrets của backend và không truy cập Docker socket.
- **Agent Runtime:** Pi là runtime chính; OMP (Oh My Pi) là runtime phụ. Cả hai chạy dưới dạng Node.js process/container độc lập do Go Agent Runtime Adapter quản lý qua giao thức JSON-RPC/stdio có phiên bản. Kiểm tra tương thích giao thức bằng smoke tests; không giả định có Go SDK trực tiếp.
- **Cách ly runtime:** Git worktree/branch cho mỗi task + Docker sandbox, một agent viết code hoạt động/task tại một thời điểm (lease/fencing và hạn mức tài nguyên sẽ phát triển ở các phase sau).
- **Chất lượng kiểm thử:** Go testing + `httptest` + `testify` (tùy chọn) + tích hợp Testcontainers/Compose; Web dùng `Vitest` và `Playwright`.
- **CI & Vận hành:** GitHub Actions (Go lint/vet/test/build, Web typecheck/lint/build, protocol conformance, security scan, Compose smoke). Logging cấu trúc (`slog` hoặc `zerolog`), OpenTelemetry, Temporal UI, lưu trữ artifact tương thích S3 (dùng local adapter trong bootstrap).
- **Triển khai local:** Docker Compose, CLI viết bằng Go (`platform doctor`, `platform worker status`).
- **Không sử dụng cho backend:** Loại bỏ Fastify, Prisma, TypeScript Temporal SDK, Turborepo cho việc build backend. `pnpm` chỉ dùng duy nhất cho workspace JS của Web và agent-runtime.

### 3.3 Kiến trúc Monorepo
- Một Git monorepo phân cấp: `apps/`, `packages/`, `contracts/`, `infra/`.
- Ba scope mã nguồn triển khai trong bootstrap:
  1. `apps/backend`: Chứa đúng 1 module Go duy nhất (`apps/backend/go.mod`). **Tuyệt đối không tạo `go.work` cho MVP.**
  2. `apps/web`: Next.js frontend.
  3. `apps/agent-runtime`: Host Node.js cho Pi/OMP.
- Một `pnpm-workspace.yaml` quản lý duy nhất các package JS/TS. Root `Makefile` là entrypoint duy nhất để điều phối Go, pnpm, Docker và CI.
- Không scaffold trước các ứng dụng `apps/desktop` hoặc `apps/mobile` trong Phase 0 (đây chỉ là quy ước roadmap). Không tạo trước các shared UI package khi chưa có consumer thực tế.

### 3.4 Tiêu chí nghiệm thu (Acceptance Criteria)
1. Ba ứng dụng được cô lập tại `apps/backend`, `apps/web`, `apps/agent-runtime`; không có root Go module hay `go.work`.
2. Root `Makefile` điều phối toàn bộ build/test/dev; Go module và pnpm workspace độc lập.
3. `contracts/openapi` cung cấp ranh giới API ổn định, tài liệu hóa rõ ràng cho các client.
4. CI nhận diện đường dẫn (path-aware) để chạy kiểm tra theo scope; thay đổi contract kích hoạt kiểm tra consumer phụ thuộc.
5. Không scaffold ứng dụng desktop/mobile hoặc package UI suy đoán trong Phase 0.
6. Clone repo sạch cài đặt được Go, Node/pnpm và build thành công API, worker, CLI, Web, agent host.
7. Go API trả về `/health` (liveness) và `/ready` (dependency readiness), phát SSE skeleton có xác thực.
8. Next.js hiển thị trạng thái sức khỏe của backend qua giao diện đã quy định.
9. Temporal worker Go chạy thành công workflow mẫu (Go Workflow → Go Activity); khởi động lại worker khi activity retry vẫn phục hồi an toàn.
10. SQL migration PostgreSQL và truy vấn sqlc hoạt động; Temporal DB và Platform DB tách biệt logic.
11. Hợp đồng Go AgentRuntime và IPC schema có version; smoke test chứng minh handshake, chuẩn hóa event, cancel, xử lý lỗi của Pi/OMP host.
12. Go CLI `platform doctor` kiểm tra dependencies/kết nối; `platform worker status` ở chế độ read-only.
13. Lint, `go vet`, Go tests, Web typecheck/lint/build, IPC conformance và secret scan pass toàn bộ trên CI.
14. Môi trường Docker Compose tái lập được; tài liệu kiến trúc, ADR, runbook đầy đủ, không lộ secret.
15. Kiểm tra ranh giới: Web không truy cập runtime nội bộ, Workflow Go không chứa side-effects bất định.

### 3.5 Bộ kịch bản kiểm thử (Test Scenarios)
- BS-01: Clone sạch, cài đặt và build toàn bộ thành công.
- BS-02: Docker Compose khởi động các dịch vụ ở trạng thái healthy.
- BS-03: Kiểm tra `/health` và `/ready`.
- BS-04: Workflow Temporal mẫu hoàn thành thành công.
- BS-05: Worker restart phục hồi activity an toàn.
- BS-06: SQL migration có tính idempotent (chạy lại không lỗi).
- BS-07: Web Next.js hiển thị API health.
- BS-08: Cấu hình sai/thiếu thì fail-safe rõ ràng.
- BS-09: Vi phạm ranh giới phụ thuộc làm CI fail.
- BS-10: GitHub Actions xanh toàn bộ.
- BS-11: Quét secret phát hiện và chặn commit test secret giả.
- BS-12: Pi host handshake / streaming / cancellation.
- BS-13: OMP host handshake / streaming / cancellation.
- BS-14: Agent IPC timeout, giải phóng tài nguyên, không rò rỉ process con.
- BS-15: Scoped Go build/test chạy từ `apps/backend`.
- BS-16: pnpm workspace chỉ cài đặt phạm vi JS.
- BS-17: Kiểm tra tính tương thích của OpenAPI contract.
- BS-18: Sửa contract kích hoạt kiểm tra consumer tương ứng.
- BS-19: Xác minh không tồn tại `go.work` và không scaffold thừa desktop/mobile app.

## 4. Phân tích

### 4.1 Yêu cầu rõ ràng
- **Mô hình kiến trúc:** Monorepo đa ngôn ngữ phân tách triệt để: Go cho core platform orchestration/backend, TypeScript cho Web UI và Agent wrapper host.
- **Ranh giới công nghệ:**
  - `apps/backend/go.mod` là module duy nhất; cấm dùng `go.work`.
  - `pnpm` chỉ quản lý các ứng dụng Node.js trong `apps/` và các package JS sau này.
  - Root `Makefile` là điểm tiếp xúc duy nhất cho dev (`make dev`, `make build`, `make test`, v.v.).
- **Temporal & Go Worker:** Determinism là bất biến trong Workflow Go; mọi thao tác mạng, database, file system, Docker, process execution phải chuyển vào Activities.
- **Database:** PostgreSQL 17, schema platform tách rời database của Temporal.
- **Phạm vi Phase 0:** Dừng ở mức khung nền tảng, smoke tests, CI, sample workflow và docs; không làm trước pipeline AI tạo code thật hay tạo PR.

### 4.2 Yêu cầu mơ hồ / Cần clarify
- **Công cụ SQL Migration:** Đang để mở lựa chọn giữa `Goose` và `golang-migrate` ("select one before implementation"). Cần chốt một công cụ chính thức.
- **Thư viện Logging trong Go:** Đang để mở giữa stdlib `log/slog` và `zerolog`. Cần thống nhất chọn một để tránh phân mảnh code.
- **HTTP Router:** Đang gợi ý `net/http` hoặc router nhẹ (`chi` preferred). Cần chốt chính thức dùng `go-chi/chi/v5`.
- **Cơ chế lưu trữ Artifact cục bộ:** Đề cập "S3-compatible artifacts (local adapter in bootstrap)" nhưng chưa nêu rõ dùng container MinIO trong Docker Compose hay một adapter lưu file cục bộ trên ổ đĩa.
- **Cơ chế xác thực cho Go API & Web:** Đề cập RBAC middleware và authenticated SSE skeleton, nhưng chưa chỉ định hình thức token (Static Bearer token qua biến môi trường, Mock JWT hay Session-based).
- **Giao thức và CLI thực tế của Pi / OMP:** Cách đóng gói Pi và OMP CLI vào Docker container hoặc môi trường host để adapter Go có thể spawn sub-process và giao tiếp qua stdio.

### 4.3 Phạm vi ảnh hưởng (Code & Repo)
- Thư mục hiện tại `/home/vif/workspace/vtx` là thư mục hoàn toàn mới, chưa có git repository hay bất kỳ file mã nguồn nào ngoài thư mục `plans/tasks/`.
- Cần khởi tạo toàn bộ cây thư mục theo `Proposed repository structure` của SPEC-001 v1.2:
  - Khởi tạo Git repo (`git init`).
  - `apps/backend/`: `go.mod`, cấu trúc thư mục `cmd/{api,worker,cli}`, `internal/{domain,application,transport,orchestration,persistence,agent,sandbox,worktree,qa,qualitygate,observability}`, `db/{migrations,queries}`, `sqlc.yaml`.
  - `apps/web/`: Next.js template với Tailwind CSS, shadcn/ui.
  - `apps/agent-runtime/`: Node.js TypeScript project host cho Pi/OMP.
  - `contracts/`: `openapi/`, `agent/`, `events/`.
  - `infra/`: `compose/`, `temporal/`, `postgres/`, `scripts/`.
  - Root: `Makefile`, `package.json`, `pnpm-workspace.yaml`, `.env.example`, `.gitignore`, `.node-version`, `README.md`, `CONTRIBUTING.md`.

### 4.4 Rủi ro / Cạm bẫy
- **Quản lý Process con của Agent:** Khi chạy Pi/OMP qua stdio từ Go supervisor, nếu tiến trình bị timeout hoặc cancel, việc `cmd.Process.Kill()` có thể không dọn sạch các tiến trình cháu (con của node/pi), dẫn tới zombie process hoặc rò rỉ tài nguyên hệ thống. Cần dùng process group (`syscall.Setpgid` và `syscall.Kill(-pgid, syscall.SIGKILL)` trên Linux).
- **Vi phạm tính Deterministic của Temporal:** Các kỹ sư có thể vô tình gọi `time.Now()`, sinh số ngẫu nhiên, đọc biến môi trường hoặc gọi I/O trực tiếp trong Go Workflow definition thay vì thông qua `workflow.Now()`, `workflow.SideEffect()` hoặc chuyển vào Temporal Activity.
- **Rối loạn quản lý dependency Monorepo:** Tránh việc vô thức tạo `go.work` ở root khi mở repo bằng IDE (VSCode / GoLand thường tự gợi ý tạo `go.work`). Cần cấu hình rõ trong `.gitignore` và test scenario BS-19 để chặn commit `go.work`.
- **Đồng bộ phiên bản công cụ dev:** Dự án yêu cầu cả Go (1.23+), Node.js (20+), pnpm (9+), Docker, docker-compose, sqlc, migration CLI. Thiếu kịch bản kiểm tra môi trường ban đầu (`platform doctor` / `make doctor`) sẽ dẫn tới lỗi không đồng nhất giữa các máy dev và CI.
- **Cách ly dữ liệu Temporal và Platform:** Nếu cấu hình dùng chung một schema hoặc một database duy nhất giữa Temporal server và ứng dụng platform sẽ gây xung đột transaction, khóa bảng và khó khăn khi nâng cấp Temporal version.

### 4.5 Phụ thuộc
- **Blocked by:** Không có (VTX-14 là task khởi đầu - Phase 0).
- **Blocks:**
  - VTX-2: Jira Ingestion & Work Item Parser.
  - VTX-3: Task Orchestration Engine.
  - VTX-4: Agent Execution Adapter & Workspace Sandbox.
  - VTX-5: Quality Gate & Test Runner.
  - Các phase sau: VTX-7, VTX-8, VTX-9, VTX-10, VTX-11, VTX-12, VTX-13.
- **Epic liên quan:** VTX-1 (`[AI Automation] Autonomous Development Pipeline — FMS.Application`).

## 5. Câu hỏi mở

1. **Lựa chọn công cụ Database Migration:** Đề xuất chốt `Goose` (hỗ trợ SQL migration nguyên bản, hỗ trợ Go migration khi cần logic phức tạp, tích hợp mượt với pgx). PO/Team có đồng ý với đề xuất này không?
2. **Lựa chọn Logging Library:** Đề xuất chuẩn hóa trên thư viện chuẩn `log/slog` (có sẵn từ Go 1.21, zero dependency, hỗ trợ JSON handler có cấu trúc cho OpenTelemetry). PO/Team có đồng ý không hay cần dùng `zerolog`?
3. **HTTP Routing Framework:** Đề xuất chốt `go-chi/chi/v5` vì 100% tương thích `net/http.Handler`, middleware phong phú, hiệu năng cao và nhẹ.
4. **Local Artifact Storage Adapter:** Trong giai đoạn bootstrap, nên chạy container MinIO trong Docker Compose (giả lập sát S3 API) hay chỉ cần Adapter ghi file cục bộ vào thư mục `infra/storage`?
5. **Cơ chế xác thực ban đầu cho Go API:** Sử dụng API Key đơn giản (Bearer token tĩnh cấu hình qua `.env`) cho bootstrap endpoint `/health`, `/ready`, `/events` hay tích hợp Mock JWT?

## 6. Đề xuất bước tiếp theo

1. **Chốt 5 câu hỏi mở** (Goose vs golang-migrate, slog vs zerolog, chi router, MinIO vs local storage, auth token) trước khi bắt tay viết code.
2. **Khởi tạo khung dự án (Scaffolding):**
   - Chạy `git init` cho workspace `/home/vif/workspace/vtx`.
   - Tạo cấu trúc thư mục, root `Makefile`, `.gitignore`, `.node-version`, `.env.example`.
   - Thiết lập `apps/backend` với `go.mod` và `sqlc.yaml`.
   - Thiết lập `apps/web` (Next.js App Router) và `apps/agent-runtime` cùng `pnpm-workspace.yaml`.
3. **Cấu hình Hạ tầng (Infra & Compose):**
   - Viết `infra/compose/docker-compose.yml` định nghĩa PostgreSQL (2 logical databases: `platform` và `temporal`), Temporal server/UI, Go API, Worker và Next.js.
4. **Triển khai Core Contracts & Sample Flows:**
   - Tạo OpenAPI spec ban đầu tại `contracts/openapi/`.
   - Triển khai endpoint `/health` và `/ready` trên Go API.
   - Triển khai Temporal Sample Workflow & Activity chứng minh worker restart an toàn.
   - Triển khai Agent Runtime IPC adapter mẫu cho Pi/OMP.
5. **Thiết lập CI GitHub Actions:**
   - Cấu hình workflow `.github/workflows/ci.yml` kiểm tra lint, test, boundary check và compose smoke theo bộ kịch bản BS-01 đến BS-19.

---
*Report chỉ phục vụ phân tích — không phải plan implement.*
