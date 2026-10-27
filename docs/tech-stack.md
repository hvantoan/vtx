# VTX Platform — Technology Stack Specification

**Document Version:** 1.0.0  
**Phase:** Phase 0 (Bootstrap / VTX-14)  
**Status:** Approved & Locked  

---

## 1. Stack Components by Surface

| Surface / Area | Technology | Version / Spec | Justification & Role |
|---|---|---|---|
| **Core Backend** | Go | 1.23+ (Toolchain 1.25) | Single `apps/backend/go.mod`. High concurrency, static binary compilation, predictable memory profile. |
| **HTTP Router** | `go-chi/chi/v5` | v5.2.1+ | 100% `net/http` compatible, idiomatic sub-routers, zero external allocation overhead, middleware chaining. |
| **Structured Logging** | Go Standard Library `log/slog` | Built-in | Structured JSON logging, zero third-party dependencies, standard context propagation, OTel log bridge ready. |
| **Database Driver** | `jackc/pgx/v5` | v5.7.0+ | Native PostgreSQL 17 wire protocol, connection pooling, high performance binary format support. |
| **SQL Type Generation**| `sqlc` | v1.30.0 | SQL-first type safety; writes standard PostgreSQL queries, generates idiomatic Go structs and repositories. |
| **Database Migrations**| `Goose` | v3.27.0+ | Native versioned SQL migration files (`.sql`), idempotent runs, out-of-process and in-binary migration support. |
| **Workflow Orchestration**| `Temporal Go SDK` | v1.33.0+ | Distributed durable state machine execution, deterministic workflow definitions, resilient activity retries. |
| **Web Frontend** | `Next.js` (App Router) + React | 15.x / React 19 | Server and Client Components, fast rendering, strict TypeScript mode, API isolation from backend secrets. |
| **Styling & Components**| `Tailwind CSS` + UI components | v3 / v4 + Lucide | Utility-first responsive styling, accessible components, clean dark/light dashboard theme. |
| **Agent Runtime Host** | `Node.js` + TypeScript | Node 20+ (v22.x) | Host adapter for Pi and OMP coding agents. Communicates over JSON-RPC 2.0 stdio with Go supervisor. |
| **Package Manager** | `pnpm` | 10.x | Scoped strictly to `apps/web` and `apps/agent-runtime` via `pnpm-workspace.yaml`. Not used for Go. |
| **Container & Local Infra**| `Docker` & `Docker Compose` | v2.38+ | Local infrastructure orchestration (PostgreSQL 17, Temporal Server, Temporal Web UI, VTX API, Web UI). |
| **Build & Orchestration**| GNU `Makefile` | 4.x+ | Sole unified developer entrypoint (`make dev`, `make build`, `make test`, `make lint`, `make doctor`). |
| **Continuous Integration**| GitHub Actions | v4 runners | Path-aware selective workflows via `dorny/paths-filter`, mandatory status check gate aggregator. |

---

## 2. Architectural Decisions (ADR Summary)

### ADR-0001: Goose as Migration Tool
- **Context:** Need predictable, idempotent database migrations for PostgreSQL 17.
- **Decision:** Use `Goose` v3 with standard SQL files (`.sql` with `-- +goose Up` and `-- +goose Down` directives).
- **Consequences:** Zero runtime dependency on complex ORMs; migrations can be applied via CLI or embedded Go code during container startup.

### ADR-0002: Go Standard Library `log/slog`
- **Context:** Structured JSON logging across all backend services with context metadata.
- **Decision:** Standard library `log/slog` with a custom JSON handler writing to stdout.
- **Consequences:** Eliminates external logger dependency churn, provides standard context propagation and log level filtering (`DEBUG`, `INFO`, `WARN`, `ERROR`).

### ADR-0003: Chi Router for HTTP Transport
- **Context:** REST endpoints (`/health`, `/ready`), authenticated SSE (`/events`), and OpenAPI compliance.
- **Decision:** Use `go-chi/chi/v5`.
- **Consequences:** Native `http.Handler` interoperability; straightforward middleware for CORS, request ID, structured request logging, session-cookie auth for operators, and bearer-token auth for machine actors.

### ADR-0004: NDJSON Framing for Agent Runtime IPC
- **Context:** Go supervisor spawns and coordinates Node.js Pi/OMP host over stdio.
- **Decision:** Line-delimited JSON (NDJSON) JSON-RPC 2.0.
- **Consequences:** Automatic line-based resynchronization upon transient corrupt payload without blocking pipe buffers. Supervisor manages child process groups (`syscall.Setpgid`) to cleanly kill subtrees upon cancellation or timeout.

### ADR-0005: Strict Go Module Isolation without `go.work`
- **Context:** Monorepo houses Go backend alongside TypeScript frontends.
- **Decision:** Single `go.mod` at `apps/backend/go.mod`. Explicit `GOWORK=off` in root Makefile and CI.
- **Consequences:** Prevents local development workstation leakage, IDE accidental `go.work` creation, and ensures reproducible builds across all environments.
