---
title: VTX Platform Monorepo Bootstrap
date: 2026-10-14
summary: Bootstrapped Go + Next.js + Temporal + Node Agent Runtime monorepo for VTX-14
---

# VTX Platform Monorepo Bootstrap

## What happened
Bootstrapped the foundational monorepo for the VTX Autonomous Development Pipeline platform (Phase 0 / Task VTX-14).
Created the multi-language repository layout separating Go core orchestration, TypeScript Next.js dashboard, and Node.js Agent Runtime host.
Enforced strict architectural boundaries: single `go.mod` without `go.work`, scoped `pnpm` workspace for frontend and agent runtime, and root Makefile coordination.
Implemented and verified:
- `apps/backend`: Chi HTTP router (`/health`, `/ready`, `/api/v1/system/status`, `/api/v1/workers`, authenticated `/api/v1/events` SSE), Temporal Go worker with deterministic `SampleWorkflow` and heartbeat-resuming `SampleActivity`, Go CLI (`platform doctor`, `platform worker status`), Goose SQL migrations, pgxpool persistence and sqlc type generation.
- `apps/agent-runtime`: Node.js TypeScript host managing Pi and OMP coding agents over JSON-RPC 2.0 stdio with line-delimited JSON framing, event streaming, task cancellation, and Unix process group cleanup (`syscall.Setpgid`).
- `apps/web`: Next.js 15 App Router operations cockpit displaying real-time system liveness, dependency readiness, worker counts, and live SSE event telemetry.
- `contracts/`: OpenAPI 3.1 specification, Agent IPC JSON-RPC schemas, and platform event schemas.
- `infra/`: Docker Compose local infrastructure for PostgreSQL 17 (logical isolation: `vtx_platform` vs `temporal`) and Temporal Server.
- CI & Quality Gates: Path-aware GitHub Actions workflow and `./infra/scripts/check-boundaries.sh`.

## Decisions
- ADR-0001: Goose v3 chosen for SQL-first database migrations.
- ADR-0002: Go standard library `log/slog` chosen for structured JSON logging.
- ADR-0003: `go-chi/chi/v5` chosen for HTTP routing.
- ADR-0004: NDJSON JSON-RPC 2.0 chosen for Agent Runtime stdio IPC with Unix process group isolation.
- ADR-0005: Strict Go module isolation without `go.work`.
- Resolved code review findings: HTTP Server `WriteTimeout: 0` for persistent SSE streams, EventSource `?token=` parameter authentication with `crypto/subtle.ConstantTimeCompare`, buffer scaling and channel drain for `bufio.Scanner` in Go agent supervisor, and `cmd.Cancel` process group signal escalation.

## Next steps
- VTX-2: Implement Jira Ingestion & Work Item Parser.
- VTX-3: Implement Task Orchestration Engine using Temporal Workflows.
- VTX-4: Implement Agent Execution Adapter & Workspace Sandbox.
- VTX-5: Implement Quality Gate & Test Runner.

> Historical work record — not durable authority. Prefer docs/specs/ADRs for current decisions.
