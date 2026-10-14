# VTX Platform — Autonomous Development Pipeline

[![CI Pipeline](https://github.com/hvantoan/vtx/actions/workflows/ci.yml/badge.svg)](.github/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.23%2B-blue)](apps/backend/go.mod)
[![Next.js](https://img.shields.io/badge/Next.js-15-black)](apps/web)
[![Temporal](https://img.shields.io/badge/Temporal-1.26-purple)](infra/compose/docker-compose.yml)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-17-blue)](infra/postgres)

Enterprise-grade monorepo foundation for the **VTX Autonomous Development Pipeline** (Phase 0 / Task VTX-14).
Provides end-to-end task intake, Temporal workflow orchestration, isolated Git worktree execution, Pi/OMP coding agent supervision over JSON-RPC 2.0 stdio, and real-time observability telemetry.

---

## 1. Monorepo Architecture

```
.
├── Makefile                      # Unified developer entrypoint (build, test, dev, doctor)
├── package.json                  # Root pnpm script definitions
├── pnpm-workspace.yaml           # Scoped strictly to JS/TS apps (apps/web, apps/agent-runtime)
├── .env.example                  # Environment configuration template
├── .node-version                 # Pinned Node.js version (22.23.2)
├── contracts/
│   ├── openapi/v1/openapi.yaml   # OpenAPI 3.1 specification for Backend REST API
│   ├── agent/v1/agent-ipc.json   # JSON-RPC 2.0 schemas for Pi/OMP Host IPC
│   └── events/v1/events.json     # SSE and platform event schema specifications
├── apps/
│   ├── backend/                  # The ONLY Go module in repo (github.com/vtx/platform)
│   │   ├── cmd/api/              # HTTP Chi server (/health, /ready, /api/v1/events)
│   │   ├── cmd/worker/           # Temporal worker running deterministic workflows
│   │   ├── cmd/cli/              # Platform CLI (doctor, worker status)
│   │   ├── db/migrations/        # Versioned Goose SQL migrations
│   │   ├── db/queries/           # sqlc type-safe query definitions
│   │   └── internal/             # Domain, orchestration, persistence, supervisor
│   ├── web/                      # Next.js 15 App Router Frontend Dashboard
│   └── agent-runtime/            # Node.js TypeScript host for Pi & OMP coding agents
├── docs/                         # Architecture, ADRs, research, design guidelines
└── infra/
    ├── compose/                  # Docker Compose (PostgreSQL 17, Temporal, API, Web)
    ├── postgres/                 # PostgreSQL multi-database init (vtx_platform + temporal)
    └── scripts/                  # Boundary and conformance verification scripts
```

---

## 2. Prerequisites

| Tool | Minimum Version | Installation Check |
|---|---|---|
| **Go** | 1.23+ (Toolchain 1.25+) | `go version` |
| **Node.js** | 20+ (Pinned: 22.23.2) | `node -v` |
| **pnpm** | 10.x | `pnpm -v` |
| **Docker & Compose** | Docker 28+, Compose v2.38+ | `docker compose version` |
| **Goose** | v3.27+ | `goose -version` |
| **sqlc** | v1.30+ | `sqlc version` |

To verify your local environment against all prerequisites, run:
```bash
make doctor
```

---

## 3. Quickstart & Local Development

### Step 1: Clone and Configure Environment
```bash
cp .env.example .env
```

### Step 2: Install JS Dependencies
```bash
pnpm install
```

### Step 3: Start Infrastructure Services
Start PostgreSQL 17 and Temporal Server with Docker Compose:
```bash
make compose-up
```

Verify service readiness:
```bash
./infra/scripts/wait-for-it.sh localhost 49432 30
./infra/scripts/wait-for-it.sh localhost 49233 30
```

### Step 4: Run Database Migrations
```bash
make migrate-up
```

### Step 5: Build and Run Services
Compile all binaries and web assets:
```bash
make build
```

Run backend API:
```bash
./apps/backend/bin/api
```

Run Next.js Web Dashboard:
```bash
pnpm --filter @vtx/web dev
# Access cockpit at http://localhost:49030
```

---

## 4. Testing & Verification

Run the full monorepo test suite (Boundary checks, Go tests with `-race`, Agent runtime tests, Web tests):
```bash
make test
```

Run individual scoped test suites:
```bash
make test-boundaries   # Monorepo architectural boundary checks
make test-backend      # Go unit & integration tests
make test-agent        # Agent runtime vitest suite
make test-web          # Next.js dashboard vitest suite
make lint              # Go vet + ESLint
make typecheck         # TypeScript strict typecheck across web & agent-runtime
```

---

## 5. Core API Endpoints

| Endpoint | Method | Auth | Description |
|---|---|---|---|
| `/health` | GET | Public | Liveness probe returning HTTP 200 and uptime |
| `/ready` | GET | Public | Dependency readiness probe (PostgreSQL 17 + Temporal) |
| `/api/v1/system/status` | GET | Public | Go runtime engine status, goroutines, commit |
| `/api/v1/workers` | GET | Public | Active Temporal worker status and task queues |
| `/api/v1/events` | GET | Bearer / `?token=` | Real-time Server-Sent Events stream for telemetry |

OpenAPI 3.1 specification available at `contracts/openapi/v1/openapi.yaml`.

---

## 6. Architectural Boundary Invariants

1. **Single Go Module:** Exactly one `go.mod` is located at `apps/backend/go.mod`. `go.work` is strictly forbidden in repository commits.
2. **Deterministic Workflows:** Temporal Go workflows in `internal/orchestration/workflows` are strictly deterministic. All I/O, network, database, file system, process execution, and random/time generation must happen in Temporal Activities.
3. **Subprocess Isolation:** The Go supervisor manages child Node.js Pi/OMP processes using Unix process groups (`syscall.Setpgid`) to ensure clean tree teardown without orphaned zombie processes upon cancel or timeout.
4. **Isolated Web Surface:** `apps/web` communicates exclusively with the backend via HTTP/SSE contracts. It never has access to backend database secrets or the Docker daemon socket (`/var/run/docker.sock`).
