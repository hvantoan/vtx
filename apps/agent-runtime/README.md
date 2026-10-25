# Agent Runtime Host (`apps/agent-runtime`)

The Agent Runtime Host is a standalone Node.js process spawned per run attempt by the Go Agent Supervisor (`apps/backend/internal/agent/supervisor.go`). It speaks JSON-RPC 2.0 over stdio (NDJSON) and drives a real coding-agent CLI inside the run's git worktree.

- **Contract**: `contracts/agent/v1/agent-ipc.json` (v1.1). **Design**: `docs/design/vtx-7-agent-runtime.md`.
- **stdout is protocol only.** Diagnostics go to stderr and `host/log`; `console.*` is bridged to `host/log` in the entrypoint.

## Protocol

| Message | Behaviour |
|---|---|
| `initialize` | Negotiates `min(requested, 1.1)` when the major matches, otherwise `-32001`. Runs the adapter preflight (`<cli> --version`); failure is `-32003`. Must be the first message. |
| `task/execute` | Validates params (`-32602`), canonicalizes `workspace_path` (`-32004`), enforces one task per host (same id `-32600`, other id `-32002`). Streams `task/event` (per-task `seq` from 1, payload capped at 64 KiB). Sends exactly one terminal result: `succeeded`, `failed`, or `cancelled`. |
| `task/cancel` | Notification (a request form is also answered). Aborts the task with the given reason. The terminal `cancelled` result is still sent. |
| `timeout_ms` | Host timer aborts the task with reason `timeout`. |
| `host/heartbeat` | Emitted every `heartbeat_ms` (default 5000) while a task runs, only when 1.1 was negotiated. |
| `shutdown` / `exit` | `shutdown` cancels the active task, waits for its terminal result, then replies `{}`. `exit` terminates the process. Stdin EOF also cancels and exits. |
| Envelope faults | `-32700` parse error (the line is skipped and a `host/log` warning is emitted), `-32600` invalid request, `-32601` unknown method. Notifications never receive error responses. |

## Adapters

| Adapter | File | Type |
|---|---|---|
| `PiCliAdapter` | `src/adapters/pi.ts` | **Real, primary.** Runs `pi --mode json -p --no-session <prompt>` with the worktree as cwd. Binary override: `VTX_PI_BIN`. |
| `OmpCliAdapter` | `src/adapters/omp.ts` | **Real, secondary.** Runs `omp --mode json -p --no-session --cwd <worktree> <prompt>`. Binary override: `VTX_OMP_BIN`. |
| `TestDoubleAdapter` | `tests/support/test-double-adapter.ts` | **TEST DOUBLE.** Simulation for protocol tests only. Reports `test_double: true`. Lives under `tests/`, so it is never compiled into the build output or reachable from the host's production path. |

Real adapters always report `test_double: false`. They run the CLI as a child process of the host so the supervisor's process-group kill reaches it. Cancel sends SIGTERM, then SIGKILL after 3 s. After success, a `commit` artifact is reported when `HEAD` advanced. The supervisor still captures the authoritative SHA itself (design D7-3).

The agent child receives only allowlisted environment variables (`PATH`, `HOME`, `TMPDIR`, `LANG` plus `env_allowlist` names). Values of secret-looking variables are redacted from events and results.

## Session registry

`src/sessions/registry.ts` defines the runtime-side persistence shape matching `apps/backend/db/migrations/00007_agent_runtime.sql` (`agent_sessions`, `agent_events`). It is idempotent on `(run_id, attempt)` and `(session_id, seq)`. `InMemorySessionRegistry` is the default test store, and `FileSessionRegistry` is an append-only JSONL log. Set `VTX_SESSION_REGISTRY_DIR` to enable it in the host process. The Go service owns the Postgres rows.

## Tests

```bash
pnpm --filter @vtx/agent-runtime test                     # hermetic: test double + scripted fake agent (tests/fixtures/fake-agent.sh)
VTX_SMOKE_PI=1 pnpm --filter @vtx/agent-runtime test      # also runs the real `pi` smoke test (needs `pi` on PATH and model credentials)
```

`VTX_SMOKE_PI_ARGS` adds extra Pi CLI flags (for example `--model x`) to the smoke run.
