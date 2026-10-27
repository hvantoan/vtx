# VTX Platform Bootstrap — Brainstorm Contract

**Status:** Locked (Auto Mode)  
**Date:** 2026-10-14  
**Target:** Monorepo Bootstrap (Phase 0 / VTX-14)  
**Epic:** VTX-1 ([AI Automation] Autonomous Development Pipeline — FMS.Application)  

---

## 1. Product Outcome & Scope

Initialize a clean, production-grade, extensible backend-first monorepo for the VTX Autonomous Development Pipeline platform.
The bootstrap delivers:
1. Isolated multi-language monorepo architecture:
   - `apps/backend`: Single Go module (`go.mod`), Chi HTTP API, Temporal Go Worker, CLI (`platform doctor`, `platform worker status`), PostgreSQL persistence (pgx + sqlc + Goose).
   - `apps/web`: Next.js frontend (TypeScript, Tailwind CSS, shadcn/ui), displaying live backend health/readiness and system metrics.
   - `apps/agent-runtime`: Node.js/TypeScript host executing coding agents (Pi primary, OMP secondary) over a versioned JSON-RPC stdio protocol.
2. Versioned interface contracts (`contracts/openapi`, `contracts/agent`, `contracts/events`).
3. Local development infrastructure via Docker Compose (PostgreSQL 17 with logical DB separation for Platform and Temporal, Temporal Server/UI).
4. Root `Makefile` as the sole operational entrypoint for all developer workflows.
5. Path-aware CI pipeline with GitHub Actions verifying lint, vet, tests, build, boundary constraints, and contract conformance.

---

## 2. Constraints & Non-Goals

### 2.1 Technical Constraints
- **Core Backend:** Go 1.23+ (pinned in `apps/backend/go.mod`). Router: `go-chi/chi/v5`. Logging: `log/slog` with structured JSON output.
- **Strict Go Module Boundary:** Exactly one Go module at `apps/backend`. **NEVER create `go.work`** in the repository.
- **Workflow Determinism:** Temporal Go workflows must be strictly deterministic. All I/O, network, database, process execution, and random/time generation must happen in Temporal Activities.
- **Database & Migration:** PostgreSQL 17. Migrations managed by `Goose` with versioned idempotent SQL files. Persistence queries via `pgx/v5` and `sqlc`.
- **Database Logical Isolation:** Temporal server tables and VTX Platform tables must reside in separate logical databases (`temporal` vs `vtx_platform`).
- **Web App:** Next.js (App Router), React, TypeScript strict mode, Tailwind CSS. Web client consumes Go REST & SSE; web must never access internal backend secrets, database credentials, or Docker socket.
- **Agent Runtime Host:** Node.js 20+ / pnpm. Communicates with Go supervisor via bidirectional JSON-RPC 2.0 over stdio with process-group isolation (`syscall.Setpgid`) to prevent zombie processes on cancel/timeout.
- **Dependency Isolation:** `pnpm` is strictly scoped to `apps/web` and `apps/agent-runtime`. Backend build does not depend on Node/pnpm.

### 2.2 Explicit Non-Goals (Phase 0)
- No automated live coding or draft PR creation against real business repos (FMS.Application pipeline runs in future phases: VTX-2 through VTX-5).
- No speculative desktop (`apps/desktop`) or mobile (`apps/mobile`) applications.
- No premature shared UI library packages in `packages/` before actual multi-consumer demand.
- No live production deployment in Phase 0; target is local Docker Compose and path-aware CI reproducibility.

---

## 3. Recorded Assumptions (Decisions for Open Questions)

1. **Migration Tool:** `Goose` (v3) chosen for native SQL migrations, pgx compatibility, and out-of-the-box CLI support.
2. **Logging Library:** Standard library `log/slog` with JSON handler for zero-dependency structured logging and OpenTelemetry compatibility.
3. **HTTP Router:** `go-chi/chi/v5` for 100% `net/http` compatibility, zero magic, high throughput, and robust middleware chaining.
4. **Local Artifact Storage:** Local filesystem storage adapter (`infra/storage`) implementing an `ArtifactStore` interface with S3-compatible design abstraction.
5. **Authentication Mechanism:** App-hosted operator login (username/password, argon2id, server-side `vtx_session` cookie) for human endpoints such as the SSE stream and dashboard controls; machine principals use per-actor bearer tokens. `/health` (liveness) and `/ready` (dependency readiness) remain unauthenticated. This supersedes the earlier static `API_AUTH_TOKEN` decision.
6. **Agent Runtime Isolation:** Go supervisor manages child Node processes using Unix process groups (`syscall.Setpgid(0, 0)` and `syscall.Kill(-pgid, syscall.SIGKILL)` on Linux) to guarantee prompt cleanup.

---

## 4. Acceptance Criteria & Test Scenarios

### 4.1 Acceptance Criteria (AC 1–15)
1. Monorepo isolates `apps/backend`, `apps/web`, `apps/agent-runtime`; no root `go.mod` or `go.work`.
2. Root `Makefile` coordinates `dev`, `build`, `test`, `lint`, `clean`.
3. `contracts/openapi` provides stable, documented API contracts for clients.
4. Path-aware CI triggers only relevant test suites based on changed paths and contracts.
5. No desktop, mobile, or speculative shared packages scaffolded.
6. Clean clone can setup, build, and run all services with documented prerequisites.
7. Go API serves `/health` (liveness) and `/ready` (database/temporal readiness), and authenticated SSE `/events`.
8. Next.js dashboard displays backend liveness and readiness states.
9. Temporal worker runs sample workflow (`SampleWorkflow` → `SampleActivity`) with safe recovery on simulated activity retry.
10. Goose PostgreSQL migrations and sqlc queries execute idempotently; Temporal and Platform DBs are logically separated.
11. AgentRuntime IPC schema v1.0 validated; smoke tests prove handshake, streaming, cancellation, and error handling for Pi/OMP host.
12. Go CLI `platform doctor` verifies environment/dependencies; `platform worker status` reports read-only health.
13. CI passes Go vet/lint/test, Web typecheck/lint/build, IPC conformance, and secret scanning.
14. Docker Compose environment is reproducible; architecture documentation, ADR, and runbooks provided without secrets.
15. Dependency boundary checks enforce no cross-boundary leaks (Web cannot import backend or access Docker socket; Go workflows contain no non-deterministic side-effects).

### 4.2 Test Scenarios (BS-01 to BS-19)
- **BS-01:** Clean clone, install, and full build passes.
- **BS-02:** Docker Compose services start healthy.
- **BS-03:** Verify `/health` and `/ready` HTTP probe responses.
- **BS-04:** Temporal sample workflow completes successfully.
- **BS-05:** Worker restart recovers activity safely.
- **BS-06:** SQL migrations are idempotent.
- **BS-07:** Next.js UI reflects backend health status.
- **BS-08:** Missing configuration yields clear fail-safe error messages.
- **BS-09:** Boundary violation tests trigger build/CI failure.
- **BS-10:** GitHub Actions CI workflow passes completely.
- **BS-11:** Secret scanning detects and rejects committed secrets.
- **BS-12:** Pi host handshake, streaming, and cancellation.
- **BS-13:** OMP host handshake, streaming, and cancellation.
- **BS-14:** Agent IPC timeout and process tree cleanup without zombie leaks.
- **BS-15:** Scoped Go build/test runs strictly inside `apps/backend`.
- **BS-16:** pnpm workspace installs only Node/JS dependencies.
- **BS-17:** OpenAPI contract validation passes.
- **BS-18:** Contract change triggers dependent consumer validation.
- **BS-19:** Boundary validation confirms absence of `go.work` and unneeded mobile/desktop packages.
