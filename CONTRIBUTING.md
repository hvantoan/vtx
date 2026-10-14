# Contributing to VTX Platform

Thank you for contributing to the VTX Autonomous Development Pipeline platform.
This repository strictly enforces architectural boundaries and code quality standards to ensure long-term stability and maintainability.

---

## 1. Architectural Invariants (Non-Negotiable)

1. **NO `go.work`**: Never create or commit `go.work` or `go.work.sum` files. `apps/backend/go.mod` is the single source of truth for Go dependencies.
2. **Path Isolation**:
   - `apps/backend` contains Go code only.
   - `apps/web` contains Next.js frontend code only. Never import Go code or internal backend packages.
   - `apps/agent-runtime` contains Node.js / TypeScript code only.
3. **No Speculative Scaffolding**: Do not scaffold desktop (`apps/desktop`), mobile (`apps/mobile`), or premature shared UI packages until explicit multi-consumer demand is approved.
4. **Boundary Validation**: Every PR must pass `./infra/scripts/check-boundaries.sh`.

---

## 2. Development Workflow

### Adding Database Migrations (Goose)
Create a new migration file under `apps/backend/db/migrations/`:
```bash
goose -dir apps/backend/db/migrations create add_new_feature_table sql
```
Write `-- +goose Up` and `-- +goose Down` blocks using pure SQL. Ensure migrations are idempotent (`CREATE TABLE IF NOT EXISTS`, etc.).

### Updating SQL Queries (sqlc)
1. Add or edit SQL queries in `apps/backend/db/queries/`.
2. Generate Go models and interfaces:
```bash
cd apps/backend && sqlc generate
```
Never manually edit files under `apps/backend/internal/persistence/db/`.

### Temporal Workflow Guidelines
- **Strict Determinism**: Workflows must never call `time.Now()`, `rand.Int()`, or make direct HTTP/database/file system calls.
- Use `workflow.Now(ctx)`, `workflow.Sleep(ctx, d)`, and delegate all external operations to Temporal Activities.
- Activities should record heartbeats (`activity.RecordHeartbeat`) and support resume via `activity.GetHeartbeatDetails(ctx)`.

### Agent Runtime Host Guidelines
- Communication is strictly line-delimited JSON (NDJSON) JSON-RPC 2.0.
- Diagnostics, warnings, and informational logs must be written to `stderr` (`logStderr`), keeping `stdout` reserved strictly for valid JSON-RPC frames.
- Tasks must support cancellation via `AbortSignal`.

---

## 3. Pull Request Checklist

Before submitting a PR, verify:
- [ ] `make test-boundaries` passes with zero violations.
- [ ] `make test` passes all Go, agent runtime, and web tests.
- [ ] `make lint` and `make typecheck` pass with zero errors.
- [ ] `make build` compiles all binaries and bundles cleanly.
- [ ] No secrets, keys, or internal credentials committed.
