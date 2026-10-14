# AGENTS.md — Repository Guidance for Coding Agents

This file instructs AI coding agents (Pi, OMP, Claude Code, Codex, Cursor) operating on the `vtx` repository.

---

## 1. Monorepo Structure & Build Rules

- **Root Makefile**: Always use `make <target>` from the repository root as the primary operational entrypoint.
- **Go Backend**: Single Go module at `apps/backend`. Never create or modify `go.work`. Run commands as `go -C apps/backend <cmd>`.
- **Node.js Apps**: Managed with `pnpm` under `pnpm-workspace.yaml`. Do not use npm or yarn.
- **No Speculative Scaffolding**: Never create `apps/desktop`, `apps/mobile`, or premature shared UI packages.

---

## 2. Coding Conventions

### Go (`apps/backend`)
- Router: `go-chi/chi/v5`
- Logging: standard library `log/slog` (structured JSON logging). Do not import external loggers.
- Persistence: `pgx/v5` + `sqlc`. Do not write raw SQL in Go files; put SQL in `db/queries/` and run `sqlc generate`.
- Temporal Workflows: Strict determinism. All I/O must happen inside Temporal Activities.
- Error Handling: Return wrapped errors (`fmt.Errorf("...: %w", err)`). Never discard errors silently.

### TypeScript (`apps/web` & `apps/agent-runtime`)
- Strict TypeScript (`strict: true`).
- **NEVER use `: any` or `as any`**. Use `unknown`, domain types, generics, or type guards.
- Prefer `Promise.withResolvers()` over `new Promise((resolve, reject) => ...)`.
- No wall-clock timers in test files (`ts-no-test-timers`). Use fake timers or await promise signals.
- Avoid redundant `clearTimeout` / `clearInterval` guards.

---

## 3. Verification Protocol

Before completing any task, run:
```bash
make test-boundaries
make vet
make typecheck
make test
make build
```
Ensure all checks pass cleanly with zero warnings or errors.
