# Advice v1.1 — Triển khai VTX-3 (AUTO-02 Task Registry & State Machine)

> Generated: 2026-10-19 | Input: https://vtxsolution.atlassian.net/browse/VTX-3 (To Do, SPEC-AUTO-02 v1.1) + `plans/tasks/vtx-3-auto-02-foundation-task-registry-va-state-machine.md`

> Verify base: repo `vtx` tại HEAD `85a7d9a`. Mọi khẳng định về repo bên dưới do phiên này tự đọc/grep, không chép lại từ report phân tích.

> Phạm vi: advisory, đã cập nhật sau architecture review ngày 2026-10-19. Không thay thế `ak:plan`/`ak:cook`. Thông tin repository tại `85a7d9a` là bằng chứng của bản Advice gốc, **chưa được tái kiểm chứng trong lần chỉnh sửa này**.
> **Verdict cập nhật: CONDITIONAL APPROVAL — xử lý 4 mục P1 trước khi thực thi migration và hoàn thiện runtime.**

---

## 0. Quyết định đã chốt trong phiên (decision log)

| # | Câu hỏi | Chốt | Hệ quả |

|---|---|---|---|

| D1 | Vì sao làm ngay | **Chặn VTX-4/5/7** — cần đúng shape schema/contract sớm | Ưu tiên hình dạng dữ liệu + service; cho phép ghi BLOCKED cho phần chỉ phục vụ compliance |

| D2 | Consumer tiêu thụ registry qua đâu | **HTTP API nội bộ + auth** | Thêm transport + middleware actor + openapi + test 401/403/409 + cursor pagination |

| D3 | Nhịp merge | **Một nhánh liền mạch, merge khi đủ DoD** | Không có mốc trung gian cho VTX-4/5/7; phải chấp nhận thời gian chờ |

| D4 | Độ sâu state machine | Đủ transition map, nhưng stage chưa có subsystem thật là contract-only và không thể vận hành thành công | Không ghi audit thành công giả cho coding/QA/gate/PR |

| D5 | Auth + actor | **Token theo actor**, reuse pattern env-token đã có | Cần biến env mới fail-closed; handler bỏ field `actor` của body |
| D6 | Bootstrap migration | Không DROP trực tiếp ba bảng cũ | Chuyển schema an toàn hoặc registry_tasks riêng; backup và reviewer approval |
| D7 | Outbox ACK | ACK theo consumer và event type | `task_run.*` pending đến khi có consumer downstream |
| D8 | Process boundary | Registry-worker không phụ thuộc Temporal | Tách mode/entrypoint dù dùng chung Docker image |

---

## 1. Decisions and prerequisites before coding

1. **Migration safety gate (P1):** tuyệt đối không `DROP TABLE tasks/task_events/agent_sessions` chỉ dựa vào grep thấy 0 consumer. Chụp row counts, foreign keys, dependency ngoài repository, xác nhận backup/restore và dữ liệu thực tế. Ưu tiên dùng tên bảng mới (`registry_tasks`/tên đã thống nhất) để tránh phá bootstrap; nếu yêu cầu giữ `tasks`, phải có kế hoạch chuyển dữ liệu có kiểm chứng, rollback và approval riêng. Comment deviation trên Jira **không thay thế approval dữ liệu**.
2. **Outbox delivery ownership (P1):** một registry consumer identity có N replicas. Source `execution_intent.created.v1` được ACK cho đúng consumer sau transaction. Event `task_run.*` giữ pending cho consumer tương lai; không được ACK khi chưa có consumer. Xác định rõ `published_at` hiện có biểu diễn trạng thái của consumer VTX-3 trong Phase 1 và thêm receipt/delivery mapping theo subscriber trước khi mở rộng fanout.
3. **Process isolation (P1):** dùng cùng image nhưng khởi chạy registry dispatcher qua entrypoint/mode riêng (`worker registry` hoặc `registry-dispatcher`); không để Temporal connect failure buộc registry dispatcher exit. Có thể chạy `worker temporal` độc lập.
4. **Operational state guards (P1):** implement bảng chuyển trạng thái cho contract nhưng disable hoặc validate evidence không thể giả mạo cho các stage chưa triển khai; không cho `quality_gate → pr_draft` hay `pr_draft → completed` chỉ vì actor hợp lệ. Test non-operational stage bị chặn rõ ràng.
5. **Credentials:** provision 4 token role-bound (`REGISTRY_ACTOR_SCHEDULER_TOKEN`, `REGISTRY_ACTOR_ORCHESTRATOR_TOKEN`, `REGISTRY_ACTOR_OPERATOR_TOKEN`, `REGISTRY_ACTOR_POLICY_TOKEN`) theo cấu hình fail-closed; actor lấy từ authenticated principal, không lấy từ request body.
6. **Evidence:** chạy migration trên PostgreSQL populated thực tế, TR-01..25 với kết quả PASS/BLOCKED và CI exact SHA. Quyết định migration naming phải được phản ánh vào sqlc/Go hợp đồng.

---

## 2. Verdict

Spec **tốt và triển khai được**, không cần viết lại. Nhưng nó rộng hơn vẻ ngoài: một ticket đang gánh đồng thời migration cần đảm bảo backward compatibility, state machine, outbox dispatcher, DLQ/redrive, auth, contracts và 25 test case. Phần khó **không** nằm ở state machine — bảng transition chỉ là một `map` ~40 dòng; phần khó là **outbox dispatcher + evidence 2-instance + đường test có DB thật**, mà cả ba thứ này repo hiện chưa có (xem §5). Với D3 (merge một lần), VTX-4/5/7 nằm chờ suốt thời gian đó — đây là rủi ro lịch, không phải rủi ro kỹ thuật.

Hai điều tích cực đã verify: hạ tầng test cần dùng **đã tồn tại** (compose stack + nginx + `--scale backend=2` + evidence theo `github.sha` + goose/sqlc pinned), và contract payload của VTX-2 **đã đủ field** mà §2 yêu cầu nhập. Đừng dựng lại thứ nào trong số này.

---

## 2A. Four non-negotiable review gates (P1)

| Gate | Requirement | Proof to attach |
|---|---|---|
| G1 No destructive migration | Preserve all bootstrap and VTX-2 data; any rename/copy migration must have tested rollback, external-dependency inventory and explicit approval | Row counts + schema diff + populated DB up/down/restore evidence |
| G2 Consumer-specific delivery | Exactly-one persisted effect on replay; distinguish source ACK from outbound task_run.* delivery; no global ACK for future subscribers | Crash-after-commit test; pending outbound event query; subscriber receipt contract |
| G3 Runtime isolation | Registry worker functions with PostgreSQL when Temporal unavailable; crashes/restarts do not kill API or Temporal worker | Compose independent process test; Temporal-down registry intake test |
| G4 Evidence-gated stage transitions | Future stages are contract-only in Phase 1; actor alone cannot advance into completion/PR success | Negative tests for unsupported downstream transition and forged evidence |

---

## 3. What you should do (thứ tự bắt buộc)

| # | Việc | Vì sao thứ tự này |

|---|---|---|

| 1 | **Compatibility matrix** (Appendix A) → commit `tests/evidence/` + comment Jira | §0.A mandatory; là input cho reviewer |

| 2 | Migration additive `00004_task_registry.sql` (không DROP bootstrap) + `db/queries/registry.sql`, `db/queries/outbox.sql` → `sqlc generate` | Mọi thứ sau phụ thuộc hình dạng schema |

| 3 | `internal/domain/registry.go`: states/actions/actors + transition map + typed error | Thuần dữ liệu, test được không cần DB |

| 4 | `internal/application/registry_service.go`: `TransitionRun` + `ConsumeIntent` | Hai transaction duy nhất của hệ thống |

| 5 | `internal/application/outbox_dispatcher.go` + **service compose `worker`** (§5.1) | Bước 4 xong mới có handler để gọi |

| 6 | `internal/transport/http/handlers_registry.go` + `ActorAuthMiddleware` + openapi | Trên cùng service, không sửa service |

| 7 | `contracts/events/v1/events.json` (+2 event type), slog + query metrics, runbook | Sau khi hành vi đóng băng |

| 8 | Test: Go integration (build tag) + shell harness cho TR cần nhiều process (§5.2) | Cuối — nhưng test I-01/I-03/I-05 viết **trước** bước 3 |

Ba điểm kỹ thuật quyết định thành/bại:

### 3.1 `TransitionRun` = `SELECT … FOR UPDATE` rồi CAS, không phải CAS đơn thuần

§0.D bắt thứ tự lỗi `not_found → stale_version → stale_lease → already_terminal → invalid_transition/forbidden`. Một `UPDATE … WHERE version=$2` không phân biệt được năm trường hợp đó (chỉ biết "0 rows"). Trong **một** transaction:

1\. `SELECT state, version, lease_owner, lease_epoch, lease_expires_at FROM task_runs WHERE run_id=$1 FOR UPDATE`

2\. phân loại lỗi trong Go theo đúng thứ tự §0.D

3\. `UPDATE task_runs … WHERE run_id=$1 AND version=$2`

4\. `INSERT task_run_events` + `INSERT outbox_events`

5\. commit

`FOR UPDATE` cũng chính là thứ làm TR-07 tất định: người đầu commit `version+1`, 19 người còn lại block rồi đọc ra version mới → `stale_version`.

### 3.2 Không có "epoch=0 bypass"

§0.D cấm nó. Điều kiện epoch phải là biểu thức tường minh hai nhánh: `(lease_owner IS NULL AND \<action thuộc policy operator trên queued/blocked>) OR (lease_owner IS NOT NULL AND lease_epoch = $expected AND lease_expires_at > now())`. Viết `if expectedEpoch == 0 { skip }` đúng là lỗ hổng mà spec gọi tên.

### 3.3 Dispatcher claim → consume → ACK; ACK thuộc một consumer

1\. **Claim** (tx ngắn, commit trước mọi I/O): `SELECT … FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL AND event_type='execution_intent.created.v1' AND (claimed_until IS NULL OR claimed_until < now()) AND (next_attempt_at IS NULL OR next_attempt_at <= now()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT n` → set `claimed_by`, `claimed_until`, `attempt+1` → COMMIT.

2\. **Xử lý** ngoài transaction: decode payload, validate đủ field bắt buộc (§2: thiếu field ⇒ lỗi schema/version, không sinh giá trị thay thế).

3\. **Consume** (one tx): `inbox_consumptions` insert `ON CONFLICT DO NOTHING` → task upsert theo `(jira_site_id, jira_issue_id)` → run `queued` v0 **hoặc** `deferred_intents` → outbound event → COMMIT. Sau đó **tx riêng** mới `SET published_at = now()` (ACK).

Giữ ACK là bước riêng để kiểm chứng crash-after-commit-before-ACK (TR-20). `published_at` của outbox gốc không được diễn giải thành global delivery confirmation cho tất cả consumers. Với event `task_run.*` chưa có subscriber, giữ pending và định nghĩa consumer-specific receipt/delivery contract cho các phase sau.

---

## 4. What you shouldn't do

- **Đừng tạo bảng DLQ riêng + CLI `redrive`.** Quarantine là 2 cột trên `outbox_events` (`dead_lettered_at`, `dead_letter_reason`); redrive là `UPDATE outbox_events SET dead_lettered_at=NULL, dead_letter_reason=NULL, attempt=0, next_attempt_at=NULL WHERE outbox_id=$1` — một câu SQL trong runbook. TR-22 chỉ đòi "inspectable with reason/redrive procedure", không đòi bảng hay binary mới.

- **Đừng xây routing framework cho outbox.** §0.E chỉ cấm self-reconsume. Dispatcher lọc cứng `event_type='execution_intent.created.v1'` ⇒ event `task_run.*` do registry phát **không thể** quay lại chính nó theo cấu trúc; không cần bảng route, không cần `consumer_name` cho dispatcher.

- **Đừng thêm cột `jira_site_id`/`issue_id`/`changelog_id`/`correlation_id` vào `execution_intents`.** Payload outbox đã có đủ 10 field §2 yêu cầu (verify Appendix A) và VTX-2 ghi payload cùng transaction. Thêm cột = thêm migration + backfill để không đổi hành vi nào.

- **Đừng làm bảng checkpoint cho backfill.** Chính `outbox_events WHERE published_at IS NULL` là checkpoint: không dispatcher nào từng chạy (0 chỗ ghi `published_at` trong repo), nên mọi intent cũ đều còn row chưa publish ⇒ **bật worker trên DB populated chính là backfill**. Idempotency do `task_runs.intent_id UNIQUE` + `inbox_consumptions` UNIQUE lo.

- **Đừng import Temporal/Git/Docker vào code registry.** TR-16 test đúng cái này, và `go.temporal.io/sdk v1.49.0` đã nằm sẵn trong `go.mod` nên rất dễ lỡ tay.

- **Đừng thêm `prometheus/client_golang`.** `internal/observability` chỉ có `logger.go` (slog) và repo không có scraper. Metrics phase 1 = slog line có cấu trúc + 2 query (`active runs by state`, `outbox lag`). Thêm client khi có nơi scrape — quyết định đảo ngược được, không cần chặn.

- **Đừng dùng `webhook_receipts` thay `inbox_consumptions`.** Khác khoá, khác mục đích; I-02 yêu cầu UNIQUE(`consumer_name`, `source_event_id`).

- **Đừng để cursor phân trang dựa `created_at`.** §7 nói ordering theo run version; TR-23 test "stable on equal timestamps" — cursor = version đúng theo cấu trúc, không cần tie-breaker.

- **Đừng viết `make test-backend` chạm DB.** Nó là `go test -v -race ./...` thuần unit; test cần DB phải nằm sau build tag (§5.2).

---

## 5. What could be better / more efficient

### 5.1 Chỗ chạy dispatcher — repo chưa có (điểm mù của advice đầu tiên, đã verify)

Bằng chứng:

- `infra/compose/docker-compose.yml` services: `postgres, temporal, temporal-ui, backend, gateway, web` — **không có `worker`**; `docker-compose.prod.yml` cũng không.

- `docker-compose.e2e-scale.yml` chỉ `ports: !reset []` cho `backend`.

- `apps/backend/Dockerfile` **đã** build sẵn `/app/worker`, nhưng `CMD ["/app/api"]`.

- `cmd/worker/main.go` hiện chỉ dựng Temporal worker (`SampleWorkflow`/`SampleActivity`).

- `infra/scripts/test-temporal-restart.sh` có chạy `bin/worker` như process host + SIGKILL, nhưng **không** được gọi trong `ci.yml`.

⇒ Nếu không chốt, TR-03 (2 instance), TR-08 (crash/restart), TR-12/TR-22 (claim/lease/DLQ/redrive) **không có chỗ sinh evidence**.

**Chốt: thêm service `registry-worker` vào compose; runtime độc lập với Temporal và `cmd/api`.** Dùng cùng image nếu phù hợp, nhưng cần bổ sung mode/entrypoint tách biệt; ví dụ minh họa (không giả định mode đã tồn tại):

```yaml
registry-worker:
  build:
    context: ../../apps/backend
    dockerfile: Dockerfile
  command: ["/app/worker", "registry"] # implement mode này
  restart: unless-stopped
  depends_on:
    postgres: { condition: service_healthy }
  environment:
    DATABASE_URL: ${DATABASE_URL}
    # thêm các env thật theo existing configuration
```

**Không** thêm `temporal` vào `depends_on` cho registry-worker; kiểm thử Temporal unavailable nhưng registry vẫn xử lý được source event.

Lý do, theo thứ tự quan trọng: (1) service không publish port ⇒ `--scale registry-worker=2` chạy được ngay, không cần override mới (khác `backend`, phải `!reset` ports); (2) TR-08 cần SIGKILL dispatcher mà không giết API, nhúng vào `cmd/api` thì test crash đập luôn HTTP surface; (3) prod hiện **không deploy Temporal worker nào cả** — delivery loop sẽ không có nhà ở prod; (4) mode registry phải tránh Temporal init/dial và có signal shutdown/health độc lập; đây không phải thay đổi ~10 dòng được đảm bảo.

**"Một designated dispatcher" không mâu thuẫn với 2 replica.** TR-03 viết nguyên văn "concurrent duplicate consumption on **two instances** -> 1 run" ⇒ spec **bắt buộc** ≥2 process. §0.E cấm "competing consumers which each acknowledge the same VTX-2 event", nghĩa là cấm hai **consumer identity** khác nhau cùng ack một event; an toàn đến từ cùng `consumer_name` + `SKIP LOCKED` + claim lease + `inbox_consumptions` UNIQUE. N-replica cùng `consumer_name` là đúng thiết kế.

### 5.2 Repo chưa có đường test nào chạm DB thật

- `make test-backend` = `go test -v -race ./...` — thuần unit, không DB. Không có file `*_test.go` nào mang `//go:build integration`.

- Job `backend-integration` chỉ: compose up (`--profile e2e`, services `backend gateway jira-stub postgres`) → resolve `JT_PG_PORT` vào `$GITHUB_ENV` → `goose up/down-to 0/up` → sqlc drift check → **shell harness** `test-jira-webhook.sh | tee jt-evidence.log` → render prod overlay assert → upload `jt-evidence-${{ github.sha }}`.

- `test-jira-webhook.sh` đã làm sẵn đúng thứ TR-07/AC-05 cần: `--scale backend=2` với `e2e-scale` rồi scale về 1, có helper `ok`/`bad`/`note`, evidence log.

⇒ Dùng **hai** đường, đừng chế đường thứ ba:

| Loại TR | Đường chạy | Cụ thể |

|---|---|---|

| TR-01/02/04/05/06/09/10/13/15/17/21/25 (một process, gọi repo/service trực tiếp) | **Go integration test** | `//go:build integration`, DSN từ `TEST_DATABASE_URL`; target `make test-integration`; CI step mới ngay sau bước migrations, tái dùng `JT_PG_PORT` đã export |

| TR-03/07/08/20/22 (cần ≥2 process, cần SIGKILL) | **shell harness mới** `infra/scripts/test-registry-outbox.sh` | copy helper của `test-jira-webhook.sh`; `--scale registry-worker=2` cho TR-03; `docker compose kill -s SIGKILL registry-worker` cho TR-08; `psql` assert DLQ/redrive cho TR-22; `--scale backend=2` + 20 writer qua gateway cho TR-07/AC-05 |

`make test-backend` không đổi (build tag loại integration test khỏi `./...`), `make test` giữ nguyên tốc độ.

### 5.3 Các cắt giảm nhỏ khác

| Việc | Cách làm rẻ hơn | Đổi lại |

|---|---|---|

| Backfill | auto, không checkpoint, không job riêng | 0 |

| DLQ + redrive | 2 cột + 1 câu UPDATE trong runbook | row poison vẫn nằm trong `outbox_events` ⇒ mọi query "còn việc" phải kèm `AND dead_lettered_at IS NULL` |

| Transition map | `map[transitionKey]transition` trong `internal/domain` | không có state-machine DSL — đúng, đừng có |

| Auth | extend `BearerAuthMiddleware` sẵn có, thêm resolve actor | không phân biệt token hết hạn/thu hồi (chưa cần) |

| Lease trong test | set `lease_owner/lease_epoch/lease_expires_at` bằng SQL trong integration test | không có API cấp lease — nhưng §5 nói VTX-5 sở hữu, nên đây là **đúng**, không phải nợ |

| Package layout | machine vào `internal/domain`, service vào `internal/application` | không có package `registry` riêng; ít file hơn, đúng convention repo |

| Contracts | `contract-conformance` chỉ chạy `redocly lint openapi.yaml` + `JSON.parse` hai file schema | không có cross-check openapi ↔ handler; phải tự kỷ luật sync |

---

## 6. My take and how to get there

Làm đúng thứ tự bảng ở §3, **giữ toàn bộ scope đã chốt** (D2/D4), dùng 6 chặng nội bộ trong cùng một nhánh để luôn có thứ chạy được — không phải để merge sớm:

1\. **Schema + invariant, không service.** `00004` (Appendix A) + `registry.sql` + sqlc. Test ngay bằng SQL/Go integration: I-01 unique identity, I-03 partial index bị vi phạm khi chèn run nonterminal thứ hai (test qua **2 connection DB**, không phải 2 goroutine), I-05 version=0. Nếu I-03 không biểu diễn được bằng partial index thì phải biết ở đây, không phải ở TR-19.

2\. **Service + machine, chưa dispatcher.** `TransitionRun` với FOR UPDATE + CAS + 5 typed error + atomic audit/outbox. Test 20 writer bằng goroutine cùng process trước (nhanh, tất định), rồi mới lên 2 process ở chặng 4.

3\. **Dispatcher + compose `worker`.** Claim/lease/attempt/backoff/quarantine; test replay ×3, crash trước ACK (dừng worker giữa tx consume và tx ACK), poison → `dead_lettered_at`.

4\. **HTTP + auth + read API.** `ActorAuthMiddleware` resolve token → `principal.actor`; handler **bỏ** field `actor` khỏi body, chỉ dùng principal ⇒ spoof bị 403. Cursor theo version.

5\. **Evidence.** `make test-integration` + `test-registry-outbox.sh` trong job `backend-integration`; artifact `registry-evidence-${{ github.sha }}`.

6\. **Runbook + matrix + deviation comment.** TR nào thiếu môi trường → ghi BLOCKED đúng luật ticket, không PASS hình thức.

---

## 7. Benefits

- VTX-4/5/7 có hợp đồng ổn định: `task_id` bất biến theo `(jira_site_id, jira_issue_id)`, `run_id` UUID, `version` cho CAS, `lease_epoch` cho fencing — ba ticket sau không phải tự chế identity/lease.

- `outbox_events` từ chỗ chết thành hàng đợi tự chạy: bật worker là toàn bộ intent VTX-2 cũ được xử lý, không cần job backfill.

- Mọi thay đổi state có audit append-only + outbox trong cùng transaction ⇒ không có nhánh "đã đổi state nhưng mất event".

- Replay an toàn theo cấu trúc (`intent_id` unique + receipt unique), không nhờ "nhớ đã xử lý chưa".

- Tái dùng nguyên hạ tầng CI/compose/harness đã có ⇒ chi phí evidence thấp hơn nhiều so với cảm giác "25 test".

---

## 8. Trade-offs

- **D3 (merge một lần) có giá thật:** VTX-4/5/7 không bắt đầu được cho tới khi cả 25 TR + HTTP + DLQ xong. Nó thôi đúng khi có người ở VTX-4/5/7 đang rảnh chờ — lúc đó rẻ hơn là slice, và chi phí chuyển hướng là rebase một nhánh lớn.

- **D2 (HTTP) là chi phí thuần cho mục tiêu đã nêu:** VTX-4/5/7 là Go in-process, sẽ không gọi HTTP. Nó chỉ đáng khi đã có caller ngoài-process thật (UI/Opera/dịch vụ khác ngôn ngữ). Nếu chưa có, phần này không đóng góp gì cho D1.

- **Ba bảng bootstrap:** không DROP trực tiếp. Grep 0 consumer chỉ là tín hiệu ở phạm vi source repo; không chứng minh bảng rỗng hay không có consumer ngoài repo.

- **Quarantine bằng cột:** phải nhớ `AND dead_lettered_at IS NULL` ở **mọi** chỗ hỏi "còn việc" — quên một chỗ là dispatcher quay lại đánh vào poison. Đổi lại: không bảng thứ hai, không CLI, không binary mới.

- **Metrics bằng slog:** AC-10 nói "audit/metrics… observable"; nếu reviewer đọc là "phải có scrape endpoint" thì phải thêm prometheus client. Thêm sau là additive, không phá gì.

- **Không cấp lease API:** `begin` không chạy thật được cho tới khi VTX-5 tồn tại — đúng boundary §1, nhưng nghĩa là AC-07/TR-09/TR-21 chỉ chứng minh được bằng SQL test. Phải nói rõ trong evidence để reviewer không tưởng đã có scheduler.

- **Thêm service `worker`:** thêm một process phải vận hành ở prod (restart policy, log, alerting) và mode registry phải tách khỏi luồng init Temporal; đây là điều kiện nghiệm thu chứ không phải trade-off chấp nhận được. Đổi lại: dispatcher có nhà ở prod và scale được miễn phí.

---

## 9. Unverified

1\. **§0.E "single designated dispatcher"** = một consumer identity, N process. Đọc theo nghĩa "một process" sẽ mâu thuẫn trực tiếp với TR-03, nhưng câu chữ chưa chốt. Xác nhận bằng: hỏi lại tác giả spec, và ghi vào deviation comment.

2\. **Cơ chế đánh dấu delivered cho outbound `task_run.*` ở Phase 1; ACK phải theo consumer, không đánh dấu publish nếu chưa giao downstream.** §0.E nói "route registry-emitted events to other consumers", nhưng Phase 1 không có consumer nào ngoài registry. Đề nghị của tôi — row nằm `published_at IS NULL`, dispatcher lọc theo `event_type`, runbook ghi rõ "chưa có consumer tới VTX-5" — là **diễn giải**, chưa có câu chữ nào xác nhận. Hệ quả nếu reviewer đọc khác: cần thêm transport đẩy ra ngoài (HTTP/webhook) ngay trong ticket này.

3\. **Cách đọc AC-10/§7 về "metrics".** Tôi kết luận "slog + query" là đủ vì repo không có prometheus client và không có scraper; đây là suy luận, không phải câu chữ.

4\. **Có cần `schema_version` trong payload hay không.** Tôi kết luận không: hậu tố `.v1` trong `event_type` + filter cứng đủ để TR-13 từ chối schema lạ ("missing fields must yield schema/version error"). Đây là suy luận từ §2 + §3; contract hiện không có field version để đối chiếu.

5\. **`infra/scripts/test-temporal-restart.sh` có nên gộp vào CI luôn không.** Tôi chỉ xác định được nó tồn tại và không được CI gọi; chưa đọc hết để biết nó phụ thuộc môi trường gì.

6\. **Chưa kiểm chứng consumer ngoài repo cho bảng bootstrap `tasks`.** Tôi grep `apps/backend` (Go, trừ `internal/persistence/db`), `apps/web`, `apps/agent-runtime` → 0 hit. Không kiểm được script ops/SQL thủ công ngoài repo.

---

## 10. Work checklist

- [ ] Gate G1..G4 được xác minh và đính kèm evidence

- [ ] Compatibility matrix (draft ở Appendix A) → commit `tests/evidence/`, comment VTX-3

- [ ] Lập migration-safety evidence (row counts, backup/restore, external dependencies), không DROP bootstrap; báo Jira nếu cần destructive exception

- [ ] Comment xác nhận cách đọc "single designated dispatcher" (= N replica hợp lệ)

- [ ] Migration `00004_task_registry.sql`: PRESERVE `task_events`, `agent_sessions`, `tasks`; CREATE new registry task table nếu `tasks` không thể reuse an toàn → `task_runs` (+partial unique I-03) → `task_run_events` → `deferred_intents` → `inbox_consumptions`; ALTER `outbox_events` (claim/lease/attempt/next_attempt_at/dead_letter)

- [ ] `db/queries/registry.sql` + `db/queries/outbox.sql` → `sqlc generate`, không drift

- [ ] Go integration test (build tag): I-01, I-02, I-03 qua 2 connection, I-05

- [ ] `internal/domain/registry.go`: states/actions/actors + transition map (đủ §0.C) + typed errors

- [ ] `internal/application/registry_service.go`: `TransitionRun` (FOR UPDATE + CAS + epoch hai nhánh + atomic audit/outbox) và `ConsumeIntent` (task upsert → run hoặc deferred → receipt → outbound, một tx)

- [ ] Test goroutine: 20 writer cùng version → 1 commit; invalid/terminal → 0 mutation; injected tx failure → rollback toàn bộ

- [ ] `internal/application/outbox_dispatcher.go` + riêng mode registry trong `cmd/worker/main.go` (hoặc entrypoint khác): claim → xử lý ngoài tx → ACK; backoff+jitter; quarantine; recovery sau restart

- [ ] Thêm service `registry-worker` vào `docker-compose.yml` + `docker-compose.prod.yml`, independent of Temporal; verify `--scale registry-worker=2`

- [ ] Test: replay ×3 = 1 run; crash giữa tx consume và tx ACK không nhân bản; poison → DLQ; redrive sau khi sửa

- [ ] `handlers_registry.go` + `ActorAuthMiddleware` + 4 env token: body `actor` khác principal → 403; read API cursor theo version

- [ ] `contracts/events/v1/events.json` (+`task_run.created.v1`, `task_run.state_changed.v1`) + `openapi.yaml`; chạy `redocly lint` local

- [ ] slog có correlation/intent/run/event ID + outbox lag/retry/DLQ + query `active runs by state`

- [ ] `make test-integration`; `infra/scripts/test-registry-outbox.sh`; wire cả hai vào job `backend-integration`; upload `registry-evidence-${{ github.sha }}`

- [ ] Điền bảng TR-01..25 → evidence hoặc BLOCKED

- [ ] Chạy đủ protocol của AGENTS.md: `make test-boundaries && make vet && make typecheck && make test && make build`

- [ ] Runbook: redrive SQL, backfill (= bật worker), phase-1 chưa có outbound consumer, không có lease-grant API

## 11. Success metrics

| Metric | Ngưỡng |

|---|---|

| `goose up` trên DB populated | 0 mất ID/row/constraint/outbox message đang chờ (TR-24 evidence) |

| `count(*) FROM task_runs WHERE intent_id=$1` sau replay ×3 | `1` |

| 20 writer cùng `expectedVersion` (2 process) | `1` commit, `19` `stale_version` |

| `count(*) FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL AND event_type='execution_intent.created.v1'` sau khi worker chạy hết | `0` |

| `--scale registry-worker=2` + cùng một event | `1` run (TR-03) |

| `make test-boundaries && make vet && make test && make build` + `backend-integration` | xanh tại exact SHA |

| Bảng bootstrap sau `00004` | Dữ liệu/ID gốc còn nguyên; registry schema có identity và constraints đúng §3 |

## 12. Handoff brief

```text

Objective: Triển khai VTX-3 (SPEC-AUTO-02 v1.1), có điều kiện qua 4 review gates G1..G4 — tầng Task Registry & state machine: migration 00004, một dispatcher duy nhất chạy trong service compose "worker", TransitionRun với CAS version + lease-epoch fencing, HTTP read+mutation có auth theo actor, evidence TR-01..TR-25 tại exact SHA.

Done means: goose up từ DB rỗng và từ DB populated đều sạch (không mất ID/row/outbox message đang chờ); mỗi execution_intent → đúng 1 admitted task_run state=queued khi không có run khác đang nonterminal, replay 3 lần vẫn 1 run; 20 writer đồng thời từ 2 process cùng expectedVersion → đúng 1 commit, 19 stale_version; 2 replica worker cùng consumer_name cùng nhận một event → 1 run; transition thành công thì version+1 + 1 task_run_events + 1 outbox_events trong cùng tx, transition thất bại không đổi gì; worker restart không mất pending outbox; poison → quarantine có reason + redrive được; mutation không token hoặc actor spoof → 401/403; các bảng/dữ liệu bootstrap được bảo toàn hoặc có migration + rollback đã được approve và kiểm thử; make test-boundaries && make vet && make test && make build và job CI backend-integration xanh tại exact SHA; TR thiếu môi trường ghi BLOCKED, không PASS.

Out of scope: scheduler/admission/lease allocation & renewal (VTX-5); Git worktree (VTX-4); agent execution (VTX-7); QA/gate/Draft PR (VTX-9..12); Temporal workflow; GitHub PR creation; policy evaluation thật cho pr_draft→completed (chỉ contract); promotion deferred_intent → run (VTX-5); read UI.

Constraints: Go 1.26 + chi v5 + pgx v5 + sqlc + goose, không thêm dependency mới (auth reuse BearerAuthMiddleware với token theo actor, metrics dùng slog + query); 4 bảng VTX-2 chỉ extend; 3 bảng bootstrap cũng không DROP khi chưa có migration proof và approval; không hardcode Ready to Dev/10007; không gọi mạng khi đang giữ DB lock; không import Temporal/Git/Docker vào code registry; dispatcher chạy trong service compose "registry-worker" (N replica hợp lệ, cùng consumer_name), không phụ thuộc Temporal và không nhúng vào cmd/api; make test-backend không được chạm DB (test cần DB nằm sau build tag integration); merge một lần khi đủ DoD.

Stop and ask only if:

- Bất kỳ thao tác destructive (kể cả với 3 bảng bootstrap), hoặc phát hiện dữ liệu/consumer ngoài repo, phải dừng để review migration và backup.

- Phải sửa hình dạng execution_intents/outbox_events không theo hướng additive.

- Phải thêm dependency mới ngoài nhu cầu thiết yếu, hoặc sửa lifecycle ngoài mode registry đã được duyệt.

- Phải implement lease-grant API hoặc policy completion cho pr_draft→completed (vượt boundary §1).

- Một TR không thể có evidence thật: ghi BLOCKED và báo lại thay vì hạ chuẩn test.

```

---

## Appendix A — Draft compatibility matrix (§0.A)

Verify tại HEAD `85a7d9a`, đọc trực tiếp migration `00001..00003`, `db/queries/{jira,tasks}.sql`, `internal/application/outbox_events.go`, `contracts/events/v1/events.json`.

| Hiện tại | Nguồn | VTX-3 dùng thế nào | Hành động |

|---|---|---|---|

| `tasks(id VARCHAR(64) PK, title, status, prompt, workspace_path, created_at, updated_at)` | `00001` | Xung đột tên/schema với registry mới | **PRESERVE**; VTX-3 dùng bảng mới `registry_tasks` (chốt PO 2026-10-19) — không copy/migrate, không đụng bootstrap, không viết lại `db/queries/tasks.sql` |

| `task_events(id, task_id → tasks(id) ON DELETE CASCADE, seq, event_type, payload, created_at)` | `00001` | Bảng bootstrap với FK | **PRESERVE** |

| `agent_sessions(task_id → tasks(id) ON DELETE CASCADE, …)` | `00001` | Bảng bootstrap với FK | **PRESERVE** |

| `execution_intents(intent_id UUID PK, source, issue_key, target_status_id, status CHECK pending/processing/done/failed, created_at, updated_at)` | `00002` | Nguồn `intent_id` cho `task_runs.intent_id UNIQUE`; không thêm cột | Giữ nguyên, không ALTER |

| `jira_webhook_inbox(event_id, jira_site_id, issue_id, issue_key, changelog_id, target_status_id, outcome, payload_hash, request_id, correlation_id, intent_id, …)` + UNIQUE`(jira_site_id,issue_id,changelog_id,target_status_id)` | `00002` | Nguồn đối chiếu authoritative cho `issue_id`/`jira_site_id`/`changelog_id` nếu cần | Giữ nguyên |

| `outbox_events(outbox_id UUID PK, intent_id UUID NOT NULL → execution_intents ON DELETE CASCADE, event_type VARCHAR(64), payload JSONB, published_at, created_at)` + partial index `(created_at) WHERE published_at IS NULL` | `00002` | Hàng đợi dispatcher claim; cũng là đích ghi event `task_run.*` | **ALTER ADD**: `claimed_by`, `claimed_until`, `attempt INT NOT NULL DEFAULT 0`, `next_attempt_at`, `dead_lettered_at`, `dead_letter_reason`. Giữ nguyên cột/index cũ |

| `webhook_receipts(...)` | `00003` | Không liên quan | Giữ nguyên |

| Payload `execution_intent.created.v1`: `event_id, intent_id, issue_id, issue_key, jira_site_id, changelog_id, target_status_id, occurred_at, occurred_at_source, correlation_id` | `internal/application/outbox_events.go` | **Đủ** mọi field §2 yêu cầu ⇒ dispatcher đọc payload, validate bắt buộc, thiếu field ⇒ lỗi schema/version | Không đổi contract |

| `contracts/events/v1/events.json` enum: chỉ `execution_intent.created.v1` | `contracts/events/v1` | Consumer filter theo đúng giá trị này | **ADD** `task_run.created.v1`, `task_run.state_changed.v1` |

| Dispatcher outbox | — | **Không tồn tại**: chỉ `InsertOutboxEvent`, không có chỗ nào ghi `published_at` | VTX-3 implement designated dispatcher (§0.E) |

| Service chạy dispatcher | `infra/compose/*` | **Không có**: compose chỉ có postgres/temporal/temporal-ui/backend/gateway/web | **ADD** service `worker` (Dockerfile đã build sẵn `/app/worker`) |

| Migration kế tiếp | `00001..00003` | — | `00004_task_registry.sql` |

**Lưu ý nhân quả cần nêu trong matrix:** `outbox_events.intent_id NOT NULL ... ON DELETE CASCADE` buộc mọi event do registry phát phải mang `intent_id`. Điều này thoả được vì `task_runs.intent_id` là UNIQUE và mọi run đều có intent — nhưng nếu sau này có đường xoá `execution_intents`, outbox đang chờ sẽ bị xoá theo. Repo hiện không có đường xoá nào, nên CASCADE là vô hại; ghi lại để reviewer biết đây là điểm cần cân nhắc khi có retention policy.

## Appendix B — Bảng transition đã chốt (D4)

Machine values: `queued → preparing → coding → verifying → qa → quality_gate → pr_draft → completed`; nonterminal phụ: `blocked`, `retry_wait`; terminal phụ: `failed`, `cancelled`.

| From | Action | To | Actor / guard | Phase 1 |

|---|---|---|---|---|

| queued | begin | preparing | scheduler (VTX-5) + admission + lease hợp lệ | map + guard thật (lease set bằng SQL test — không có API cấp lease) |

| preparing→coding→verifying→qa→quality_gate→pr_draft | advance | stage kế tiếp | orchestrator + điều kiện stage tương ứng | map + audit; **không** side effect downstream |

| pr_draft | complete | completed | orchestrator được uỷ quyền + completion policy met | contract-only; runtime không được complete nếu policy/evidence thật chưa tồn tại |

| nonterminal trừ blocked/retry_wait | block | blocked | operator/policy + reason | lưu `resume_state` |

| blocked | resume | `resume_state` | operator approval | guard thật |

| nonterminal trừ blocked/retry_wait | schedule_retry | retry_wait | policy + budget bounded | lưu `resume_state`, `retry_due_at` |

| retry_wait | retry_due | `resume_state` | scheduler/policy, tới hạn, còn budget | guard thật |

| mọi nonterminal | cancel | cancelled | operator + reason | map + guard |

| mọi nonterminal | fail | failed | policy + reason | map + guard |

Mọi chuyển khác bị cấm, gồm self-transition và mọi chuyển đi từ terminal.

---

## Brainstorm contract (ak:brainstorm, 2026-10-19 — đối chiếu bản advice hiện tại + HEAD `85a7d9a`)

> Bản advice đã được restructure giữa phiên; mục này thay bản append trước (đã bị ghi đè) và bám hướng hiện tại: D6/§1.1 PRESERVE bootstrap + `registry_tasks`, §5.1/§12 service `registry-worker`.

Outcome / Acceptance criteria / Non-goals / Constraints: tái sử dụng §11 + §12 + G1..G4 — không lặp lại ở đây.

### Bổ sung 1 — Precheck row counts là điều kiện rẽ nhánh của G1 (chạy được ngay, không phải suy từ grep)

Query + kết quả thật trên DB dev đang chạy (vtx-postgres:49432, schema `00001..00003`):

```sql
SELECT (SELECT count(*) FROM tasks) AS tasks,
       (SELECT count(*) FROM task_events) AS task_events,
       (SELECT count(*) FROM agent_sessions) AS agent_sessions,
       (SELECT count(*) FROM execution_intents) AS intents,
       (SELECT count(*) FROM outbox_events WHERE published_at IS NULL) AS pending;
-- 2026-10-19 dev → tasks=0 task_events=0 agent_sessions=0 intents=1 pending=1
```

Quy tắc chốt (xoá nhánh mơ hồ của D6):
- Bất kỳ bảng bootstrap nào > 0 row ⇒ tái dùng tên `tasks` chỉ còn nghĩa copy/migrate có kiểm chứng + approval; **mặc định `registry_tasks`**.
- Cả 3 = 0 row ⇒ `registry_tasks` là lựa chọn rẻ nhất (không phải lo FK, không phải viết lại `db/queries/tasks.sql`); tái dùng tên `tasks` chỉ khi PO yêu cầu literal và đã có backup/restore + rollback được test.
- DB populated của CI/prod **phải chạy lại** query này và lưu `registry-migration-precheck.log` vào artifact — số liệu dev không thay thế được.
- `intents=1, pending=1` trên dev là bằng chứng sống cho "bật dispatcher = backfill": đã có intent chưa publish, không cần job/checkpoint riêng.

### Bổ sung 2 — Fail-closed token là `ValidateRegistry()`, không phải `Validate()`

Verify: `Config.Validate()` chỉ được gọi ở `cmd/api/main.go:27` (cộng test `internal/config`); `cmd/worker` và `cmd/cli` không gọi. Nhét 4 token vào `Validate()` sẽ (a) buộc `cmd/api`, compose, CI và mọi test fixture phải có token registry dù không dùng, (b) **không** fail-closed cho dispatcher (dispatcher cũng không cần actor token vì không có caller).
Chốt: thêm `ValidateRegistry()` riêng, gọi đúng chỗ dựng registry router trong `cmd/api` — fail-closed tại nơi dùng, không đụng `Validate()` hiện có.

### Bổ sung 3 — Các chốt nhỏ, đã align với bản advice hiện tại

| Điểm | Chốt | Lý do |
|---|---|---|
| Entrypoint | mode `registry` trong `cmd/worker` (`command: ["/app/worker","registry"]`), service compose tên `registry-worker` | Đúng §1.3/§5.1/§12; không sửa Dockerfile (giữ `CMD ["/app/api"]`) |
| TR-16 | assert ở **mức package**: `go list -deps ./internal/domain ./internal/application ./internal/transport/...` không chứa `go.temporal.io` | Ở mức binary là bất khả thi (cmd/worker hợp lệ vẫn import Temporal) |
| `task_events`/`agent_sessions` | PRESERVE, không ghi, không dựng bảng audit thứ hai ngoài `task_run_events` | tránh hai nguồn audit song song, đúng §1.1 |
| Cursor read API / metrics slog+query / backfill không job riêng / DLQ 2 cột | giữ nguyên §5.3 | — |

### Unresolved

§9 items 1 (cách đọc §0.E), 2 (delivered-marker cho `task_run.*`), 3 (AC-10 metrics) **đã chốt với PO** — xem bảng "Chốt với PO". Còn lại: 4 (`schema_version` trong payload — đề xuất không cần, hậu tố `event_type` + filter cứng đủ), 5 (consumer ngoài repo của bảng bootstrap — không kiểm được ngoài source repo), 6 (`test-temporal-restart.sh` có gộp vào CI) — không mục nào chặn bắt đầu plan.

### Chốt với PO (2026-10-19) — 7 quyết định, hết nhánh mơ hồ

| # | Quyết định | Hệ quả trực tiếp cho plan/cook |
|---|---|---|
| 1 | Bảng mới **`registry_tasks`** | `00004` CREATE `registry_tasks` + `task_runs` + `task_run_events` + `deferred_intents` + `inbox_consumptions`; bootstrap giữ nguyên; **không** viết lại `db/queries/tasks.sql`; 1 comment deviation trên VTX-3 (spec §3 ghi literal `tasks`) |
| 2 | Dispatcher = **mode `registry` trong `cmd/worker`** | compose `command: ["/app/worker","registry"]`, service tên `registry-worker`, không sửa Dockerfile; mode này không dial Temporal; TR-16 assert ở mức package |
| 3 | **4 env var token riêng**, fail-closed từng biến | dùng `ValidateRegistry()` (không sửa `Validate()` — chỉ `cmd/api:27` gọi nó); actor lấy từ principal, body `actor` khác principal ⇒ 403; thêm 4 biến vào `ci.env` + compose |
| 4 | Stage chưa vận hành: **chặn cứng** | map vẫn đủ §0.C, nhưng guard fail-closed cho **toàn bộ** tập hop liệt kê dưới đây — không chỉ 2 hop cuối |
| 5 | `published_at` = ACK của consumer VTX-3 | query "còn việc"/lag phải lọc thêm `event_type`; `task_run.*` giữ pending tới khi VTX-5 đến; **không** thêm bảng/cột receipt ở Phase 1 (G2 artifact = query pending theo `event_type` + test crash-after-commit-before-ACK) |
| 6 | §0.E = một consumer identity, N replica cùng `consumer_name` | TR-03 giữ nghĩa 2 instance; an toàn đến từ `consumer_name` + `SKIP LOCKED` + claim lease + `inbox_consumptions` UNIQUE |
| 7 | Metrics = slog có cấu trúc + query SQL | **không** thêm `prometheus/client_golang`; thêm sau là additive, không phá gì |

Tập hop bị chặn ở Phase 1 (guard fail-closed; gọi vào ⇒ lỗi, **0 mutation / 0 audit row**):

- `queued --begin--> preparing` — guard thật (lease + admission) nhưng không thể đạt hợp lệ vì chưa có API cấp lease (VTX-5); chứng minh bằng SQL-injected lease trong integration test — đúng lý do §8 nói AC-07/TR-09/TR-21 chỉ chứng minh được bằng SQL test.
- Mọi `advance`/`complete` từ `preparing` trở đi: `preparing→coding`, `coding→verifying`, `verifying→qa`, `qa→quality_gate`, `quality_gate→pr_draft`, `pr_draft→completed`. Negative test bắt buộc cho **cả 6 hop**; nếu chỉ chặn 2 hop cuối thì actor token vẫn đi được `queued→…→quality_gate` ⇒ phạm G4.
- Vẫn gọi bình thường: `block`, `resume`, `schedule_retry`, `retry_due`, `cancel`, `fail` trên các state thực tế đạt được (`queued`, `blocked`, `retry_wait`).

D3 (merge một lần) giữ nguyên, có bằng chứng: Jira 2026-10-19 — VTX-1/4/5/7/9 đều `To Do`, cùng assignee, không ticket nào đang chạy ⇒ chưa ai chờ shape schema, chi phí của D3 hiện bằng 0.
