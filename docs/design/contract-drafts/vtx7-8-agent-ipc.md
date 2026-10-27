# Contract Draft — Agent IPC v1.1 (VTX-7 / VTX-8)

**Status:** Draft for coordinator merge into `contracts/agent/v1/agent-ipc.json`.
**Supersedes:** additive delta on contract `1.0.0` (bootstrap, VTX-14).
**Consumers:** `apps/backend/internal/agent` (Go supervisor), `apps/agent-runtime` (Node.js host).
**SRS anchors:** §4.5 FR-AGT-001/002/003, §7 "Agent IPC", §9 VTX-14 IPC smoke + VTX-6..12 real agent integration.

---

## 1. Wire format (unchanged)

- JSON-RPC 2.0 over NDJSON: exactly one message per `\n`-terminated line on the host's **stdout**. Resync after a corrupt line is free (skip the line).
- Host **stdin** carries supervisor → host traffic; host **stderr** is diagnostics only and is never parsed.
- Request `id` is a monotonically increasing uint64 owned by the supervisor. Notifications omit `id`.
- A `task/execute` response is sent **only at terminal state**; progress travels exclusively through `task/event` notifications.

## 2. Version negotiation (new in v1.1)

- `protocol_version` is `MAJOR.MINOR`. This draft = `"1.1"`.
- Supervisor sends the highest version it supports in `initialize.params.protocol_version`.
- Host replies `initialize.result.protocol_version = min(requested, host_max)` when the **major** matches; otherwise returns error `-32001` and the supervisor MUST kill the process group.
- The negotiated version governs the whole session. A v1.0 host negotiating down to `"1.0"` means the supervisor MUST NOT send v1.1-only methods/notifications (`host/heartbeat` expectations, `exit`, extended `event_type` values remain host-side optional but supervisor tolerates absence).
- Minor-version additions are always backwards-compatible: new optional params, new enum values, new notifications. Anything breaking requires major `2.0`.

## 3. Lifecycle state diagram

```
                 spawn (Setpgid, own process group)
                    │
                    ▼
               ┌─────────┐   initialize req (10 s deadline)
               │ SPAWNED │ ───────────────────────────────┐
               └─────────┘                                ▼
                    │                              ┌──────────────┐
        stdout EOF /│ spawn error                  │ INITIALIZING │
          timeout   │                              └──────────────┘
                    ▼                                     │ result ok (negotiated version)
               ┌─────────┐                                ▼
               │  DEAD   │                          ┌─────────┐
               └─────────┘                          │  READY  │
                    ▲                               └─────────┘
                    │                                     │ task/execute
                    │                                     ▼
                    │                               ┌───────────┐  task/event* (seq 1..n)
                    │                               │ EXECUTING │ ◄────────────────────┐
                    │                               └───────────┘                      │
                    │                                     │                            │
                    │        terminal task/execute result │ succeeded|failed|cancelled │
                    │                                     ▼                            │
                    │                               ┌─────────┐  next task/execute ────┘
                    │                               │  READY  │  (v1.1 allows reuse; VTX-7
                    │                               └─────────┘   default: one execute per host)
                    │                                     │ shutdown req
                    │                                     ▼
                    │                               ┌───────────┐
                    │                               │ DRAINING  │  shutdown result → exit notif
                    │                               └───────────┘
                    │                                     │ process exits ≤ 2 s
                    ▼                                     ▼
               ┌─────────────────────────────────────────────┐
               │  DEAD — supervisor escalates at any point:  │
               │  group SIGTERM → 3 s grace → group SIGKILL; │
               │  cmd.Wait() always reaps (WaitDelay 10 s).  │
               └─────────────────────────────────────────────┘
```

Heartbeat watchdog (v1.1): while `EXECUTING`, host emits `host/heartbeat` every `heartbeat_ms`. 3 missed beats ⇒ supervisor sends `task/cancel` reason `heartbeat_timeout`; 5 missed ⇒ group SIGKILL. Heartbeats are advisory evidence for the supervisor, never a substitute for the terminal `task/execute` result.

## 4. Method and notification inventory

### 4.1 Supervisor → host

| Method | Kind | Since | Params | Result / notes |
|---|---|---|---|---|
| `initialize` | request | 1.0 | `InitializeParams` | `InitializeResult`; first and only valid first message. |
| `task/execute` | request | 1.0 | `TaskExecuteParams` | `TaskExecuteResult` at terminal state only. |
| `task/cancel` | notification | 1.0 | `TaskCancelParams` | Host aborts via `AbortController`; terminal result still required. |
| `shutdown` | request | 1.0 | `{}` | `{}`; host cancels tasks, flushes writes. |
| `exit` | notification | 1.1 | none | Host calls `process.exit(0)` within 2 s; after `shutdown` result only. |

### 4.2 Host → supervisor

| Method | Kind | Since | Params | Notes |
|---|---|---|---|---|
| `task/event` | notification | 1.0 | `TaskEventParams` | Per-task `seq` starting at 1, gap-checkable. |
| `host/heartbeat` | notification | 1.1 | `HostHeartbeatParams` | Every `heartbeat_ms` while executing. |
| `host/log` | notification | 1.1 | `HostLogParams` | Diagnostic mirror of stderr lines; raw non-JSON stdout noise SHOULD be re-emitted here instead of corrupting the stream. |

## 5. Schema delta (to merge into `definitions`)

New fields are additive; `required` lists below are authoritative for v1.1.

```jsonc
{
  "InitializeParams": {
    "type": "object",
    "required": ["protocol_version", "client_name"],
    "properties": {
      "protocol_version": { "type": "string", "pattern": "^[0-9]+\\.[0-9]+$" },   // was enum ["1.0"]
      "client_name":      { "type": "string" },
      "client_version":   { "type": "string" },
      "run_id":           { "type": "string", "format": "uuid" },                 // v1.1: correlates session rows
      "agent_type":       { "type": "string", "enum": ["pi", "omp"] },
      "capabilities": {
        "type": "object",
        "properties": {
          "cancellation": { "type": "boolean" },
          "streaming":    { "type": "boolean" },
          "heartbeat_ms": { "type": "integer", "minimum": 1000, "default": 5000 } // v1.1
        }
      },
      "env_allowlist": { "type": "array", "items": { "type": "string" } }         // v1.1: names only, never values
    }
  },

  "InitializeResult": {
    "type": "object",
    "required": ["protocol_version", "agent_type", "capabilities"],
    "properties": {
      "protocol_version": { "type": "string" },                                   // negotiated, may be < requested
      "agent_type":       { "type": "string", "enum": ["pi", "omp"] },
      "agent_version":    { "type": "string" },
      "host": {
        "type": "object",
        "properties": {
          "node_version": { "type": "string" },
          "runtime_pid":  { "type": "integer" }
        }
      },
      "adapters": {                                                                // v1.1 — FR-AGT-003 evidence
        "type": "array",
        "items": {
          "type": "object",
          "required": ["name", "version", "test_double"],
          "properties": {
            "name":        { "type": "string", "enum": ["pi", "omp"] },
            "version":     { "type": "string" },
            "test_double": { "type": "boolean" }                                  // true ⇒ simulation; may NEVER satisfy real-agent evidence
          }
        }
      },
      "capabilities": {
        "type": "object",
        "properties": {
          "streaming":      { "type": "boolean" },
          "cancellation":   { "type": "boolean" },
          "heartbeat_ms":   { "type": "integer" },                                // negotiated interval
          "max_event_bytes":{ "type": "integer", "default": 65536 }
        }
      }
    }
  },

  "TaskExecuteParams": {
    "type": "object",
    "required": ["task_id", "prompt", "workspace_path"],                          // workspace_path promoted to required
    "properties": {
      "task_id":        { "type": "string" },                                      // run_id (uuid) in v1.1
      "attempt":        { "type": "integer", "minimum": 1 },                       // v1.1: retry attempt for event correlation
      "prompt":         { "type": "string" },                                      // scope built by VTX-6 baseline
      "workspace_path": { "type": "string" },                                      // canonical run worktree; host chdirs here
      "test_baseline":  { "type": "string" },                                      // v1.1: VTX-6 baseline summary passed to agent
      "timeout_ms":     { "type": "integer", "minimum": 100 },
      "env_allowlist":  { "type": "array", "items": { "type": "string" } },
      "metadata":       { "type": "object" }                                       // v1.1: issue_key, repo_url (no secrets)
    }
  },

  "TaskExecuteResult": {
    "type": "object",
    "required": ["task_id", "status"],
    "properties": {
      "task_id":     { "type": "string" },
      "status":      { "type": "string", "enum": ["succeeded", "failed", "cancelled"] },
      "output":      { "type": "string" },
      "error":       { "type": "string" },
      "test_double": { "type": "boolean" },                                        // v1.1 — echoed from adapter, FR-AGT-003
      "artifacts": {                                                               // v1.1 — FR-AGT-001
        "type": "array",
        "items": {
          "type": "object",
          "required": ["kind", "ref"],
          "properties": {
            "kind": { "type": "string", "enum": ["commit", "file", "log", "patch"] },
            "ref":  { "type": "string" }                                           // sha / worktree-relative path / URI
          }
        }
      },
      "usage": {                                                                   // v1.1 optional telemetry
        "type": "object",
        "properties": {
          "duration_ms": { "type": "integer" },
          "tokens":      { "type": "integer" }
        }
      }
    }
  },

  "TaskEventParams": {
    "type": "object",
    "required": ["task_id", "seq", "event_type", "timestamp"],
    "properties": {
      "task_id":    { "type": "string" },
      "seq":        { "type": "integer", "minimum": 1 },
      "event_type": {
        "type": "string",
        "enum": ["stdout", "stderr", "progress", "status_change",               // 1.0
                 "tool_call", "file_diff", "error", "log"]                       // v1.1 additions
      },
      "payload":    { "type": "string", "maxLength": 65536 },
      "data":       { "type": "object" },                                          // v1.1: structured form (tool name, path, diff stat)
      "timestamp":  { "type": "string", "format": "date-time" }
    }
  },

  "TaskCancelParams": {
    "type": "object",
    "required": ["task_id"],
    "properties": {
      "task_id": { "type": "string" },
      "reason":  {
        "type": "string",
        "enum": ["operator_cancel", "operator_block", "timeout", "lease_lost",
                 "heartbeat_timeout", "shutdown", "superseded"]                    // v1.1: controlled vocabulary
      }
    }
  },

  "HostHeartbeatParams": {                                                         // v1.1
    "type": "object",
    "required": ["pid"],
    "properties": {
      "task_id":        { "type": "string" },
      "pid":            { "type": "integer" },
      "last_event_seq": { "type": "integer" },
      "timestamp":      { "type": "string", "format": "date-time" }
    }
  },

  "HostLogParams": {                                                               // v1.1
    "type": "object",
    "required": ["level", "message"],
    "properties": {
      "level":   { "type": "string", "enum": ["debug", "info", "warn", "error"] },
      "message": { "type": "string", "maxLength": 4096 },
      "meta":    { "type": "object" }
    }
  }
}
```

## 6. Error codes

Standard JSON-RPC codes are used for envelope faults; `-320xx` is the VTX application range. Task-level failure is **not** an RPC error — it is `status:"failed"` in `TaskExecuteResult`, so the stream stays parseable (decision carried over from `docs/research/agent-ipc-protocol.md`).

| Code | Name | Meaning | Retryable by supervisor |
|---|---|---|---|
| `-32700` | Parse error | Inbound line is not JSON-RPC | no — kill host |
| `-32600` | Invalid Request | Missing `jsonrpc:"2.0"`, bad shape, second `task/execute` for same `task_id` | no |
| `-32601` | Method not found | Unknown method for negotiated version | no |
| `-32602` | Invalid params | Missing `task_id`/`prompt`/`workspace_path` | no |
| `-32603` | Internal error | Unhandled host exception | yes — new host process |
| `-32000` | `task_not_found` | `task/cancel` referenced an unknown task (informational; cancel of unknown task MAY also be silently accepted) | n/a |
| `-32001` | `unsupported_protocol_version` | Major mismatch or host below required minor | no — fatal config error |
| `-32002` | `host_busy` | `task/execute` while another `task_id` is active and host is single-task | retry after drain or respawn |
| `-32003` | `agent_unavailable` | Pi/OMP CLI binary missing or failed preflight (`--version` probe failed) | no — escalate, mark session `agent_unavailable` |
| `-32004` | `workspace_invalid` | `workspace_path` missing, not a directory, or fails host-side canonical check | no — supervisor-side bug |
| `-32005` | `shutdown_timeout` | Host did not exit within 2 s of `exit` (reported by supervisor logs, not on the wire) | n/a — escalate to SIGKILL |

## 7. Discipline rules (normative)

1. **stdout is protocol.** Any host or library write to stdout that is not a valid envelope is a conformance bug; supervisor logs it as `host/log`-equivalent and skips the line.
2. **One process group, one lifecycle.** Host is spawned with `Setpgid`; all agent CLI grandchildren inherit the group and die with it. The supervisor NEVER kills only the direct child.
3. **Terminal result is mandatory.** Even after `task/cancel` or host-detected agent crash, the host MUST send the `task/execute` response before any `shutdown` processing. A missing terminal result while stdout stays open is a protocol violation and triggers escalation.
4. **`test_double` honesty.** A simulation adapter MUST report `test_double: true` in `InitializeResult.adapters[]` and echo it in `TaskExecuteResult`. The supervisor persists this on `agent_sessions.is_test_double`; evidence consumers (VTX-9/10) MUST reject `test_double` sessions for real-agent acceptance (FR-AGT-003).
5. **No secrets on the wire.** `env_allowlist` carries variable *names* only; secret values are injected by the host process environment at spawn time and are never serialized into events, results, or logs (NFR-SEC-001, NFR-AUD-001).
6. **Bounded payloads.** `payload` ≤ 64 KiB per event; hosts coalesce overflow into `progress` summaries. Supervisors enforce a 10 MiB scanner ceiling as today.

## 8. Backward compatibility & merge notes

- All v1.0 messages remain valid; v1.1 fields are optional or promoted-with-defaults except `workspace_path` (promotion to required is safe: the only producer is our supervisor, updated atomically).
- The coordinator merges §5 into `contracts/agent/v1/agent-ipc.json`, bumps `version` to `"1.1.0"`, and regenerates typed bindings in `internal/agent` + `protocol/types.ts` in the same change.
- CI conformance (BS-12 successor) feeds a recorded v1.0 session and a v1.1 session through both ends to prove negotiation-down works.
