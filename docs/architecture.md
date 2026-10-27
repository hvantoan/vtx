# VTX Platform — Architecture & Repository Structure

**Document Version:** 1.0.0  
**Phase:** Phase 0 (Bootstrap / VTX-14)  
**Status:** Approved  

---

## 1. System Context & Overview

```
                          ┌───────────────────────────┐
                          │   Developer / Operator    │
                          └─────────────┬─────────────┘
                                        │ make <target> / CLI
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│ VTX Monorepo Root                                                               │
│                                                                                 │
│   ┌─────────────────────────────────────────────────────────────────────────┐   │
│   │ apps/web (Next.js 15 + React 19 + TypeScript)                           │   │
│   │   - System Status & Readiness Dashboard                                │   │
│   │   - Real-time SSE Event Consumer                                        │   │
│   └──────────────────────┬──────────────────────────────────────────────────┘   │
│                          │ HTTP REST / SSE (Bearer Auth)                        │
│                          ▼                                                      │
│   ┌─────────────────────────────────────────────────────────────────────────┐   │
│   │ apps/backend (Go 1.23+ Module: github.com/vtx/platform)                 │   │
│   │                                                                         │   │
│   │   ┌───────────────────────┐   ┌──────────────────────────────────────┐  │   │
│   │   │ cmd/api (Chi HTTP)    │   │ cmd/worker (Temporal Worker)         │  │   │
│   │   │   - /health (liveness)│   │   - SampleWorkflow (Deterministic)   │  │   │
│   │   │   - /ready (readiness)│   │   - SampleActivity (Retry & Resume)  │  │   │
│   │   │   - /api/v1/events SSE│   └──────────────────┬───────────────────┘  │   │
│   │   └───────────┬───────────┘                      │                      │   │
│   │               │                                  │                      │   │
│   │   ┌───────────┴──────────────────────────────────┴───────────────────┐  │   │
│   │   │ internal/persistence (PostgreSQL 17 via pgx + sqlc + Goose)       │  │   │
│   │   └──────────────────────────────┬───────────────────────────────────┘  │   │
│   │                                  │                                      │   │
│   │   ┌──────────────────────────────┴───────────────────────────────────┐  │   │
│   │   │ internal/agent (Supervisor & Subprocess Group Manager)            │  │   │
│   │   └──────────────────────────────┬───────────────────────────────────┘  │   │
│   └──────────────────────────────────┼──────────────────────────────────────┘   │
│                                      │ JSON-RPC 2.0 over stdio (NDJSON)         │
│                                      ▼ (Setpgid Process Isolation)              │
│   ┌─────────────────────────────────────────────────────────────────────────┐   │
│   │ apps/agent-runtime (Node.js 20+ TypeScript Host)                        │   │
│   │   - Pi Agent Host Adapter                                               │   │
│   │   - OMP Agent Host Adapter                                              │   │
│   │   - Streaming Event Dispatcher                                          │   │
│   └─────────────────────────────────────────────────────────────────────────┘   │
└──────────────────────────────────────┬──────────────────────────────────────────┘
                                       │
                                       ▼ External Infrastructure (Docker Compose)
┌─────────────────────────────────────────────────────────────────────────────────┐
│ infra/compose                                                                   │
│   ├── postgres:17-alpine (Databases: vtx_platform, temporal)                    │
│   ├── temporalio/auto-setup:1.26.2 (Temporal Server)                            │
│   └── temporalio/ui:2.34.0 (Temporal Web UI @ :49234)                           │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Monorepo Directory Structure

```
.
├── Makefile                      # Sole top-level entrypoint for all dev/build/test actions
├── package.json                  # Root pnpm scripts & tooling
├── pnpm-workspace.yaml           # JS workspace: apps/web, apps/agent-runtime
├── .gitignore                    # Git ignore rules (explicitly forbids go.work)
├── .env.example                  # Environment configuration template
├── .node-version                 # Node version pin (22.23.2)
├── README.md                     # Monorepo overview & quickstart
├── CONTRIBUTING.md               # Developer conventions and boundary rules
├── docs/                         # Architecture, ADRs, research, specifications
├── contracts/
│   ├── openapi/v1/openapi.yaml   # OpenAPI 3.1 specification for backend REST API
│   ├── agent/v1/                 # JSON-RPC 2.0 schemas for Pi/OMP host communication
│   └── events/v1/                # Event schema definitions
├── apps/
│   ├── backend/                  # The ONLY Go module in the monorepo
│   │   ├── go.mod                # module github.com/vtx/platform
│   │   ├── go.sum
│   │   ├── sqlc.yaml             # sqlc configuration
│   │   ├── cmd/
│   │   │   ├── api/main.go       # HTTP server entrypoint
│   │   │   ├── worker/main.go    # Temporal worker entrypoint
│   │   │   └── cli/main.go       # platform doctor & status CLI entrypoint
│   │   ├── db/
│   │   │   ├── migrations/       # Goose SQL migrations (00001_init.sql...)
│   │   │   └── queries/          # sqlc source SQL queries
│   │   └── internal/             # Domain, application, transport, orchestration
│   ├── web/                      # Next.js frontend application
│   │   ├── package.json
│   │   ├── tsconfig.json
│   │   ├── next.config.ts
│   │   ├── tailwind.config.ts
│   │   └── src/
│   │       ├── app/              # Next.js App Router (page.tsx, layout.tsx)
│   │       ├── components/       # UI components (HealthCard, EventStream, MetricBadge)
│   │       └── lib/              # API client, SSE subscriber
│   └── agent-runtime/            # Node.js TypeScript host for Pi/OMP
│       ├── package.json
│       ├── tsconfig.json
│       └── src/
│           ├── host.ts           # JSON-RPC stdio server
│           ├── adapters/         # Pi & OMP runtime wrappers
│           └── protocol/         # Schema validation, framing, handlers
└── infra/
    ├── compose/
    │   └── docker-compose.yml    # PostgreSQL, Temporal, API, Web compose definition
    ├── postgres/
    │   └── init-dbs.sql          # Multi-database init script (vtx_platform + temporal)
    └── scripts/
        ├── check-boundaries.sh   # Monorepo architecture violation checker
        └── wait-for-it.sh        # Dependency readiness polling utility
```

---

## 3. Boundary Rules & Invariants

1. **Rule 1 (Single Go Module):** Exactly one Go module exists in `apps/backend`. No `go.work` or `go.work.sum` is permitted anywhere in the repository.
2. **Rule 2 (No Cross-Boundary Imports):** `apps/web` must never import files from `apps/backend` or `apps/agent-runtime`. Communication is strictly via contracts (`contracts/openapi` via HTTP/SSE).
3. **Rule 3 (No Secret Leakage):** Frontend components must never receive or bundle database connection strings, Temporal gRPC endpoints, or internal supervisor secrets.
4. **Rule 4 (Docker Socket Isolation):** Web application must never mount or have access to `/var/run/docker.sock`.
5. **Rule 5 (Workflow Determinism):** Go workflows in `internal/orchestration/workflows` must remain strictly deterministic. Network, DB, file system, process execution, and non-deterministic logic must reside exclusively in Activities.
6. **Rule 6 (Process Group Cleanup):** Go agent supervisor must spawn child Node processes with `Setpgid: true` and terminate the entire process group upon cancellation or timeout to prevent orphaned processes.

---

## 4. Jira Webhook Intake — Two-Layer Auth (VTX-2, SPEC-AUTO-01 Amendment 01)

Jira Cloud Free administrative webhooks cannot send custom `Authorization` headers or HMAC signatures. Ingress therefore uses two independent credential layers:

```
Jira Cloud ──HTTPS──▶ Nginx Gateway (:49443) ──internal HTTP──▶ Go backend (:49080)
              layer 1: URL path secret                layer 2: Bearer GATEWAY_INTERNAL_AUTH_TOKEN
```

- **Layer 1 — URL path secret (Nginx).** Jira posts to `https://<host>/webhooks/<secret>/jira`. An envsubst-templated `map` compares the path segment against `WEBHOOK_PATH_SECRET_PRIMARY`/`_SECONDARY` (rotation overlap); mismatch → `403` before any proxying. `rewrite … break` strips the secret so it never reaches Go logs or upstream services.
- **Layer 2 — injected internal token (Go).** The gateway unconditionally overwrites `Authorization` with `Bearer ${GATEWAY_INTERNAL_AUTH_TOKEN}` (client-supplied values are dropped, never forwarded). `GatewayAuthMiddleware` constant-time compares against `cfg.GatewayInternalAuthToken`.
- **Token separation is mandatory.** The webhook route must never reuse a machine actor token. `config.Validate()` fails startup if `GATEWAY_INTERNAL_AUTH_TOKEN` is empty or equals any `REGISTRY_ACTOR_*` token.
- **Published port is retained in dev.** `${VTX_API_PORT}:49080` stays published for direct API tests and the LAN proxy; the browser itself only talks to the Next.js origin. Webhook protection comes from layer 2, not from hiding the port.

---

## 5. Operator Authentication

There is exactly one human role (`operator`) and one account, seeded or reset on
the host:

```bash
make user-password          # prompts twice; writes an argon2id PHC hash
```

- **Credentials.** Username is normalized to lowercase; passwords use
  argon2id (64 MiB, t=3, p=2) with a 12-character minimum. Unknown usernames
  still pay for a dummy verification so timing does not enumerate accounts.
- **Sessions.** `POST /api/v1/auth/login` returns an opaque 32-byte token in
  an HttpOnly, SameSite=Lax `vtx_session` cookie (Secure when the request is
  HTTPS, see `SESSION_COOKIE_SECURE`). Only a SHA-256 hash is stored in
  `auth_sessions`. Sessions last `SESSION_TTL` (default 720h) and slide at
  most once per hour.
- **Rate limit.** Five failed attempts per client IP + username in a 15-minute
  fixed window return `429 too_many_attempts` with `Retry-After: 900`. The
  limiter is in-process, so each API replica enforces it independently.
- **Lifecycle.** `POST /api/v1/auth/logout` is idempotent and clears the
  cookie. `POST /api/v1/auth/password` requires the current password, revokes
  every other session, and keeps the caller signed in.
- **Authorization.** Sessions map to `domain.ActorOperator`; machine bearer
  tokens (scheduler/orchestrator/policy) are unchanged. A legacy operator
  bearer token is rejected with `401`, and `X-Operator-Id` is no longer read —
  audit identity comes from the session username.
- **Same-origin web proxy.** The dashboard calls relative `/api/*` paths. In
  compose, Next.js proxies them to `INTERNAL_API_URL` (runtime env) and streams
  SSE unbuffered; through the Cloudflare Tunnel, cloudflared routes `/api/*`
  straight to the backend. Both paths share the cookie.
