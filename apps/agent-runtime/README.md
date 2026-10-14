# Agent Runtime Host (`apps/agent-runtime`)

The Agent Runtime Host is a standalone Node.js process managed directly by the Go Backend Agent Supervisor (`apps/backend/internal/agent/supervisor.go`) via bidirectional JSON-RPC 2.0 stdio (NDJSON).

## Architecture & IPC Contract

- **Contract Specification**: `contracts/agent/v1/agent-ipc.json`
- **Protocol**: JSON-RPC 2.0 over standard I/O streams with newline-delimited JSON.
- **Process Isolation**: Process group containment (`syscall.Setpgid`), signal escalation (`SIGTERM` -> `SIGKILL`), and graceful shutdown notifications.
- **Safety**: Strict TypeScript (`strict: true`), zero `: any` or `as any`, cancellation via standard `AbortSignal`, and duplicate task guards.

## Phase 0 Bootstrap vs VTX-7 Roadmap

### Phase 0 Scope (VTX-14)
The adapters located in `src/adapters/` (`PiAgentAdapter` and `OmpAgentAdapter`) operate as **Simulation Test Doubles** (`isSimulation: true`). Their purpose in Bootstrap is to:
1. Validate the Go ↔ Node.js IPC protocol boundary and bidirectional message framing.
2. Verify task lifecycle events (`started`, `progress`, `stdout`, `stderr`, `completed`).
3. Prove graceful task cancellation through `AbortSignal` without leaking child processes.
4. Verify supervisor timeout and process group termination.

### Deferred to VTX-7
Full production coding agent execution with real Pi CLI and Oh My Pi SDKs—including dedicated Git worktree isolation, Docker sandbox execution, prompt injection defenses, and candidate code diff generation—is formally scoped and deferred to **VTX-7** (Agent Orchestration & Real Session Management).
