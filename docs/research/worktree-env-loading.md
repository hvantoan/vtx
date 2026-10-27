# Research: Tự động load env cho từng Git worktree

**Ngày nghiên cứu:** 2026-10-15
**Phạm vi:** Cơ chế tự động nạp biến môi trường (`.env`) riêng biệt cho từng git worktree trong repo `vtx` (Go backend + pnpm Node apps). Mục tiêu: `cd` vào worktree là có đúng env (port, DB name, secrets) — không cần `source` thủ công, không copy `.env` bằng tay.

## Bối cảnh trong repo

- `.gitignore` đã chặn `.env`, `.env.local`, `.env.*.local` — có nghĩa **worktree mới luôn trống env**, vì git chỉ checkout file được track. Đây là root cause của mọi vấn đề.
- Có sẵn `.env.example` ở root — template canonical để seed.
- Chưa có `.envrc`, `mise.toml`, hay tool nào quản lý env per-directory. `direnv`/`mise`/`sops` đều chưa cài trên máy dev này.

## Vấn đề cần giải

1. **Env biến mất khi tạo worktree**: file `.env*` bị gitignore → không được copy sang worktree mới. Boot app → crash `Missing required environment variable`.
2. **Xung đột tài nguyên giữa worktrees**: nếu copy nguyên `.env` sang mọi worktree, các worktree chạy cùng port, cùng DB name → collision khi chạy song song (rất phổ biến khi dùng worktree cho agent/song song hóa).
3. **Secrets**: cần cách load secrets an toàn mà không commit plaintext.

## Các phương án

### A. `direnv` + `.envrc` (khuyến nghị chính)

[direnv](https://direnv.net) hook vào shell, tự load/unload env theo thư mục hiện tại. Với worktree, `.envrc` được commit vào repo nên **mọi worktree tự động có** — chỉ cần `direnv allow` một lần cho mỗi worktree mới.

Pattern tối ưu: **derive tên worktree từ thư mục**, dùng nó để định danh resource:

```bash
# .envrc (commit vào repo — đã triển khai tại repo root)
WORKTREE_NAME="$(basename "$PWD" | tr -cd '[:alnum:]\n' | tr '[:upper:]' '[:lower:]' | tr -s '_' | sed 's/_*$//')"
export WORKTREE_NAME

# secrets/overrides (gitignored) trước, computed block sau sẽ thắng
dotenv_if_exists .env
dotenv_if_exists .env.local

# Hash ổn định → block 60-port riêng cho mỗi worktree (BASE..BASE+5),
# không cross-var collision: mọi biến dùng chung BASE, khác nhau ở offset +n
BASE=$((49000 + ($(echo -n "$WORKTREE_NAME" | cksum | cut -d' ' -f1) % 30) * 60))

export VTX_WEB_PORT=$((BASE + 0))     # vai trò tương đương 49030
export VTX_API_PORT=$((BASE + 1))     # vai trò tương đương 49080
export VTX_TEMPORAL_PORT=$((BASE + 2))
export VTX_TEMPORAL_UI_PORT=$((BASE + 3))
export VTX_POSTGRES_PORT=$((BASE + 4))
export VTX_GATEWAY_PORT=$((BASE + 5))

# Project + container/volume name theo worktree (compose đã bỏ pin
# `name: vtx` và `container_name: vtx-*`; name: trở thành ${COMPOSE_PROJECT_NAME:-vtx})
export COMPOSE_PROJECT_NAME="vtx-${WORKTREE_NAME}"

export PORT="$VTX_API_PORT"
export DATABASE_URL="postgres://vtx_user:vtx_password@localhost:${VTX_POSTGRES_PORT}/vtx_platform?sslmode=disable"
export TEMPORAL_HOST_PORT="localhost:${VTX_TEMPORAL_PORT}"
export TEMPORAL_TASK_QUEUE="vtx-task-queue-${WORKTREE_NAME}"   # compose backend cũng đọc biến này
export INTERNAL_API_URL="http://localhost:${VTX_API_PORT}"
```

### B. `worktree-env` — tool chuyên dụng cho per-worktree isolation

[worktree-env](https://github.com/iurimadeira/worktree-env) (Python, MIT, mới — 0 stars): allocate port duy nhất qua registry `~/.config/worktree-env/registry.json`, render template env (`{project}`, `{worktree}`, `{port.NAME}`), sinh `.envrc` + chạy `direnv allow` sẵn. `init` idempotent, có `gc` dọn entry của worktree đã xóa.

Phù hợp nếu cần **port allocation tuyệt đối không trùng** (dựa trên registry thay vì hash). Trade-off: tool rất mới, chưa có community; hash-based offset trong `.envrc` thuần bash đạt 99% giá trị với 0 dependency.

### C. Sync/copy `.env` giữa worktrees — giải quyết câu 1 nhưng không câu 2

Nhóm tool/scripts copy `.env` từ main repo sang worktree mới:

- **`post-checkout` hook** ([mskelton.dev](https://mskelton.dev/bytes/20230906143340), [sapsaldog.com](https://sapsaldog.com/posts/git-worktree-env-auto-copy)): hook chạy khi `git worktree add`, copy `.env*` sang worktree mới. Đơn giản nhưng hook per-repo, cần `core.hooksPath` hoặc setup thủ công.
- **[envsync](https://github.com/DukeeTheProgrammer/envsync)**, **[envtree](https://github.com/orbiteleven/envtree)**, **git-env**: CLI sync `.env` hai chiều giữa worktrees.
- **Symlink** `ln -s ../main/.env .env`: mọi worktree share một file — nhanh nhưng sửa ở đâu ảnh hưởng tất cả; triệt tiêu isolation.

Các phương án này cho mọi worktree **cùng env** → port/DB collision khi chạy song song. Chỉ đúng nếu mỗi worktree chạy một thời điểm.

### D. `mise` — thay thế hiện đại cho direnv

[mise](https://mise.jdx.dev/environments/) load env từ `mise.toml` per-directory, đồng thời quản tool versions (thay asdf/nvm). Trade-off so với `.envrc` bash: `[env]` trong mise.toml chủ yếu là giá trị tĩnh/template `{{env.*}}`; tính toán động (hash port) cần `_.file`/`_.source` hoặc script phụ. Nếu repo cần luôn tool-version management, mise là 2-trong-1; nếu chỉ cần env loading, direnv nhẹ và động hơn. Lưu ý: direnv và mise **conflict** nếu cùng hoạt động — chọn một.

### E. Secrets: `sops` + `direnv` ([thoughtsre blog](https://blog.thoughtsre.com/p/make-working-with-git-worktrees-easier))

`sops` mã hóa file secrets (YAML/env/JSON) bằng `age` key local → commit được file encrypted vào repo, worktree nào cũng có. Pattern:

- `age-keygen` tạo key ở `~/.config/vtx/age-key.txt` (ngoài repo).
- `SOPS_AGE_KEY_FILE` export qua `.env.local`.
- `.sops.yaml` map `path_regex` → public key.
- Script `with-secrets <file.enc.yaml> -- <cmd>` decrypt → flatten → export env → `exec`.

Lợi: secrets đi theo worktree qua git, không cần vault infra. Giới hạn: không rotation; mất age key = mất secrets.

## So sánh nhanh

| Phương án | Auto-load khi `cd` | Per-worktree value (port/DB) | Secrets | Dependency |
|---|---|---|---|---|
| direnv `.envrc` | ✅ | ✅ (bash động) | qua `.env.local` / sops | direnv |
| worktree-env | ✅ (sinh `.envrc`) | ✅ (registry, no collision) | ❌ | direnv + pip tool |
| post-checkout copy/symlink | ❌ (file tĩnh, cần dotenv runtime) | ❌ | giữ nguyên secrets | không |
| mise | ✅ | ⚠️ (template hạn chế) | qua file | mise |

## Khuyến nghị cho `vtx`

**Kết hợp A + E**, KISS:

1. Commit `.envrc` ở repo root: derive `WORKTREE_NAME`, offset port bằng hash, suffix DB name, `dotenv_if_exists .env.local`. Một file, mọi worktree tự động có env riêng.
2. Giữ `.env.example` làm template; secrets thật ở `.env.local` (gitignored) hoặc sops-encrypted nếu muốn commit.
3. Thêm bước `direnv allow` vào script tạo worktree (hoặc README) — bước duy nhất còn thủ công.
4. Backend Go đọc env thuần (`getEnv` trong `internal/config`) — không cần đổi code; compose đã parameterize `name: ${COMPOSE_PROJECT_NAME:-vtx}`, bỏ `container_name`/`vtx_net` pin, và `TEMPORAL_TASK_QUEUE=${TEMPORAL_TASK_QUEUE:-vtx-task-queue}` nên worker tách theo worktree.

Không cần worktree-env (hash offset đủ) hay envsync (mục tiêu là isolation, không phải đồng bộ).

## Next steps (đã triển khai)

1. ~~Viết `.envrc`~~ → đã commit tại repo root theo mẫu mục A.
2. ~~Compose isolation~~ → `name: ${COMPOSE_PROJECT_NAME:-vtx}`, bỏ 4 `container_name:` và `name: vtx_net`; script `test-jira-webhook.sh` chuyển từ `docker exec vtx-postgres` sang `docker compose exec postgres`.
3. Còn lại cho user: cài `direnv` (`apt install direnv` hoặc brew), thêm `eval "$(direnv hook bash)"` vào shell rc, `direnv allow` ở từng worktree lần đầu.
4. (Tùy chọn) Áp dụng sops nếu secrets cần đi theo repo.

## Nguồn

- [direnv — official site & stdlib](https://direnv.net) · [GitHub](https://github.com/direnv/direnv)
- [Make working with git worktrees easier using direnv and sops — thoughtsre, 2026-03](https://blog.thoughtsre.com/p/make-working-with-git-worktrees-easier)
- [Separate .env per worktree — WorktreeWise, 2026-09](https://www.worktreewise.com/git-worktree/env-files)
- [worktree-env — iurimadeira/worktree-env](https://github.com/iurimadeira/worktree-env)
- [Using Git Hooks When Creating Worktrees — Mark Skelton](https://mskelton.dev/bytes/20230906143340)
- [Auto-Managing .env Files in Git Worktrees — sapsaldog](https://sapsaldog.com/posts/git-worktree-env-auto-copy)
- [envsync](https://github.com/DukeeTheProgrammer/envsync) · [envtree](https://github.com/orbiteleven/envtree)
- [mise — environments](https://mise.jdx.dev/environments/) · [mise: migrating from direnv](https://mise.jdx.dev/direnv.html)
- [Direnv is All You Need to Parallelize Agentic Programming with Git Worktrees — waldencui.com](https://waldencui.com/post/direnv_is_all_you_need_to_parallelize_claude_code_with_git_worktrees/)

## Câu hỏi còn mở

- Hash `% 30` cho 30 block port tách biệt; xác suất 2 worktree đụng cùng block là nhỏ nhưng khác 0 — nếu cần tuyệt đối không trùng thì dùng registry kiểu `worktree-env`.
- Repo có cần per-worktree Temporal namespace không — `TEMPORAL_TASK_QUEUE` đã suffix; namespace `default` hiện giữ chung.
