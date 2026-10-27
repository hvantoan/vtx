# VTX-7 — Real Coding Agent Runtime — Design Spec

**Epic:** VTX-7 (work package: Real Coding Agent runtime)
**Status:** Design spec for implementation; contract delta in [`contract-drafts/vtx7-8-agent-ipc.md`](contract-drafts/vtx7-8-agent-ipc.md)
**Grounding:** SRS §4.5 (FR-AGT-001..003), §5 state model, §7 Agent IPC, §9 evidence; `apps/backend/internal/agent/supervisor.go`; `apps/agent-runtime/src/{host,protocol/*,adapters/*}`; `contracts/agent/v1/agent-ipc.json`; `docs/research/agent-ipc-protocol.md`.

---

## 1. Overview

VTX-7 turns the bootstrap IPC double into a real coding-agent runtime. The architecture that already exists is kept: a Go **supervisor** (`internal/agent`) spawns one Node.js **host** (`apps/agent-runtime`) per run attempt, speaks JSON-RPC 2.0 over NDJSON stdio, and kills the whole process group on teardown. What changes:

1. The Pi and OMP adapters stop simulating: each shells out to the real agent CLI inside the run's git worktree (Pi primary, OMP secondary).
2. The IPC contract moves to v1.1: version negotiation, `host/heartbeat` watchdog, `exit` handshake, `test_double` honesty flag, artifacts and richer `task/event` kinds.
3. Sessions and streamed events become durable evidence (`agent_sessions`, `agent_events`) so the audit chain `issue → intent → run → session → SHA → PR` holds (NFR-AUD-001).
4. The `preparing → coding → verifying` state-machine hops become *operational*: today `domain.transitionTable` marks every `advance` hop `operational:false` (FR-REG-007 gate G4). VTX-7 flips `preparing→coding`; VTX-8 flips `coding→verifying`. Each flip ships only together with the evidence pipeline that justifies it.

Non-goals: scheduler/admission (VTX-5), worktree lifecycle (VTX-4 — VTX-7 consumes an allocated path and never creates one), retry budget policy (VTX-11 — VTX-7 only classifies failures), QA and gate (VTX-9/10).

## 2. SRS coverage

| SRS ID | Requirement | Design decision |
|---|---|---|
| FR-AGT-001 | Agent receives scope, repo/worktree, test baseline; emits artifacts/commit SHA/logs | `task/execute` params carry `task_id=run_id`, `attempt`, `prompt` (scope built by VTX-6), `workspace_path` (allocated worktree), `test_baseline`. Result carries `artifacts[]` + `test_double`. **Commit SHA is captured by the supervisor** via `gitops.Git.HeadSHA(worktree)` after a `succeeded` result — never trusted from agent self-report — and written to `task_runs.commit_sha` inside the same `TransitionRun` bookkeeping the registry already owns. All `task/event` stdout/stderr stream persists to `agent_events` (logs). |
| FR-AGT-002 | Handshake/version negotiation, cancellation, event streaming, process-group cleanup | `initialize` request with `protocol_version` negotiation + capabilities (`heartbeat_ms`, `max_event_bytes`); mismatch → `-32001` + group kill. Cancellation = `task/cancel` notification → host `AbortController` → adapter terminates agent child (SIGTERM → 3 s → SIGKILL); terminal `status:"cancelled"` result still required. Streaming = `task/event` with per-task `seq`. Cleanup = `Setpgid` + group SIGTERM → 3 s → group SIGKILL + `cmd.Wait()` + `WaitDelay`, exactly as today's `Supervisor.Close`; v1.1 adds `shutdown`→result→`exit`→2 s→kill handshake and the heartbeat watchdog (3 missed → cancel, 5 → kill). |
| FR-AGT-003 | Pi/OMP simulation labelled test double; never real CLI evidence | `AgentAdapter.isSimulation` already exists; contract v1.1 surfaces it as `adapters[].test_double` in `InitializeResult` and `TaskExecuteResult.test_double`. Persisted on `agent_sessions.is_test_double`. Evidence consumers treat `is_test_double=true` as proof of IPC plumbing only; acceptance requiring real-agent integration MUST be reported NOT VERIFIED when only doubles ran. Real adapters return `false`; a real adapter that cannot exec the CLI fails preflight (`-32003`) rather than silently degrading to simulation. |
| FR-REG-005 | TransitionRun enforces actor authz, version CAS, lease fencing, atomic audit+outbox | Agent service never writes `task_runs` directly; every state move goes through `RegistryService.TransitionRun` with `Actor=orchestrator`, current `version`, and the run lease (owner+epoch+expiry) acquired at admission. Agent-side writes are limited to its own tables. |
| FR-REG-006/007 | State set; unimplemented transitions must not fake success | `preparing→coding` flips `operational:true` only when this service lands (evidence path exists). `coding→verifying` stays closed until VTX-8. Failed agent attempts transition via `schedule_retry`/`fail`, never by forging `advance`. |
| FR-ERR-001 | Bounded retry, terminal failure, evidence | The runtime performs **zero in-band retries**: one `task/execute` = one attempt = one host process. Classification (§6) maps outcomes to `schedule_retry` (policy actor, retry budget owned by VTX-11) or `fail`. Every attempt — including crashed/killed — leaves an `agent_sessions` row. |
| FR-SCH-002 | Lease fencing: stale worker cannot write state or apply side effects | `AgentRunService` receives `(run_id, lease_epoch)` at attempt start; `TransitionRun` CAS already rejects stale epochs. Side-effect gate: before `task/execute` returns are applied (commit SHA, advance), the service re-validates lease ownership; on `ErrWorktreeLeaseLost`/`ErrStaleEpoch` the attempt is discarded and the host is killed with reason `lease_lost`. |
| FR-SCH-003/004 | Worktree per run, isolation, crash recovery | VTX-4 owns allocation. VTX-7 only: validates `workspace_path` via `gitops.CanonicalPath` under `Layout.WorktreeRoot` before handing it to the host, refuses paths that resolve outside (error `-32004` class → non-retryable supervisor bug). Host crash never deletes the worktree; reconcile stays in VTX-4. |
| NFR-AUD-001 | Linked audit issue→intent→run→SHA→PR; no tokens/raw secrets | `agent_sessions` links `run_id`→attempt→agent/version/`test_double`/outcome; `agent_events` keeps the streamed log keyed `(session_id, seq)`; `task_runs.commit_sha` is captured independently. Host env carries secrets via `env_allowlist` *names*; values are never serialized (contract rule 5). stderr diagnostics are logged with redaction. |
| NFR-REL-001 | No lost state on crash; replay safe | Host death mid-task is detectable (stdout EOF / heartbeat loss) → session marked `crashed`, run goes `retry_wait`/`failed` via policy — never silently `completed`. `agent_events` writes are append-only and idempotent on `(session_id, seq)`. |
| NFR-SEC-001 | Least privilege, redaction, fail-closed | Agent child inherits only allowlisted env (`PATH`, `HOME`, agent auth vars configured by ops). Supervisor runs host as the worker user — no privilege escalation, no new network egress assumptions beyond what Pi/OMP CLIs need. Spawn failures fail closed (no partial session: row written as `failed` before exec where possible). |
| NFR-OPS-001 | Observable health/operations | slog fields: `run_id`, `session_id`, `agent_type`, `attempt`, `pid`, `pgid`, heartbeat misses, escalation steps. Session rows expose live attempt state for the VTX-13 dashboard (`agent_sessions.status`). |
| NFR-MAINT-001 | Contract conformance at exact SHA | Contract v1.1 is schema-merged by coordinator; CI conformance suite replays golden NDJSON transcripts through both Go supervisor and TS host at the tested commit. |
| NFR-CON-001 | Concurrent writer CAS | Not directly owned; satisfied transitively — all shared-state writes ride on `TransitionRun` CAS + lease fence. |
| NFR-PERF-001 / NFR-AVAIL-001 | SLO/RPO/RTO TBD | No thresholds invented here; design only notes measurement points (spawn latency, event throughput, time-to-kill) for the future baseline. |
| §7 Agent IPC | JSON-RPC 2.0 stdio, streaming, cancellation | Realized as contract v1.1 — see `contract-drafts/vtx7-8-agent-ipc.md` (full method list, lifecycle, error codes, negotiation). |

## 3. Data model / DDL deltas (future `00007_agent_runtime.sql`)

Goose, additive-only, mirrors 00004/00005 conventions. `attempt` aligns with `task_runs.attempt` (incremented per retry).

```sql
-- +goose Up
-- +goose StatementBegin
-- VTX-7: durable record of every agent host process spawned for a run.
-- One row per attempt; the UNIQUE is the idempotency key for spawn retries.
CREATE TABLE IF NOT EXISTS agent_sessions (
    session_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    attempt      INT  NOT NULL,
    agent_type   TEXT NOT NULL CHECK (agent_type IN ('pi', 'omp')),
    agent_version TEXT,
    protocol_version TEXT NOT NULL,          -- negotiated version, e.g. '1.1'
    -- FR-AGT-003: false = real CLI; true = simulation double. Evidence
    -- consumers (VTX-9/10) must reject true rows for real-agent acceptance.
    is_test_double BOOLEAN NOT NULL,
    status       TEXT NOT NULL CHECK (status IN (
        'starting','ready','running','succeeded','failed',
        'cancelled','crashed','killed','agent_unavailable')),
    worktree_path TEXT NOT NULL,
    pid          INT,
    exit_reason  TEXT,                        -- cancel reason or escalation step
    error_code   TEXT,                        -- e.g. '-32003' / classify() code
    error_message TEXT,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT agent_sessions_attempt_unique UNIQUE (run_id, attempt)
);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_run ON agent_sessions (run_id);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_active
    ON agent_sessions (status) WHERE status IN ('starting','ready','running');

-- VTX-7: append-only stream of task/event notifications (the run's agent log).
-- (session_id, seq) unique ⇒ replay/idempotent append on supervisor restart.
CREATE TABLE IF NOT EXISTS agent_events (
    event_id    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id  UUID NOT NULL REFERENCES agent_sessions(session_id) ON DELETE CASCADE,
    run_id      UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    seq         INT  NOT NULL,
    event_type  TEXT NOT NULL CHECK (event_type IN (
        'stdout','stderr','progress','status_change',
        'tool_call','file_diff','error','log')),
    payload     TEXT NOT NULL DEFAULT '',      -- capped at 64 KiB by contract
    data        JSONB,                          -- structured form (v1.1)
    event_ts    TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT agent_events_seq_unique UNIQUE (session_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_agent_events_run ON agent_events (run_id, event_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS agent_events;
DROP TABLE IF EXISTS agent_sessions;
-- +goose StatementEnd
```

Companion queries (`db/queries/agent.sql`): `InsertAgentSession`, `UpdateAgentSessionStatus`, `GetAgentSession`, `AppendAgentEvent` (`ON CONFLICT (session_id, seq) DO NOTHING`), `ListAgentEvents` (paginated by `event_id`), `ListActiveAgentSessions` (reconcile/orphan sweep).

Optional outbox delta (recommended, owner decision below): emit `agent_session.finished.v1` on terminal session for dashboard drill-down; payload = session row projection. Not required for correctness — `task_run.state_changed.v1` already carries state.

## 4. Go package / API design

### 4.1 `internal/agent` (extend — process/protocol boundary)

Keep `supervisor.go` as the transport owner; split typed API into `client.go`; new `lifecycle.go` owns the heartbeat watchdog and ordered shutdown; `errors.go` classifies outcomes.

```go
// client.go — typed surface over the existing raw Call/Notify.
type Client struct{ sup *Supervisor }

func (c *Client) Initialize(ctx context.Context, p InitializeParams) (InitializeResult, error)
func (c *Client) ExecuteTask(ctx context.Context, p TaskExecuteParams) (TaskExecuteResult, error)
func (c *Client) CancelTask(taskID string, reason CancelReason) error   // notification
func (c *Client) Events() <-chan TaskEvent                              // unchanged channel
func (c *Client) Shutdown(ctx context.Context) error                    // shutdown req → exit notif → escalate

// lifecycle.go — wraps Supervisor with negotiated-behavior guards.
type HostConfig struct {
    Command       string            // e.g. ["node", "dist/host.js"]
    Args          []string
    Env           map[string]string // pre-filtered to env_allowlist
    HeartbeatMiss int               // cancel threshold (default 3)
    HeartbeatKill int               // kill threshold  (default 5)
    InitTimeout   time.Duration     // default 10s
}

// errors.go — sentinel classification used by AgentRunService.
var (
    ErrVersionMismatch  = errors.New("agent: unsupported protocol version")   // -32001
    ErrAgentUnavailable = errors.New("agent: CLI unavailable")                // -32003
    ErrWorkspaceInvalid = errors.New("agent: workspace path invalid")         // -32004
    ErrHostBusy         = errors.New("agent: host busy")                      // -32002
    ErrHostDied         = errors.New("agent: process exited mid-task")        // stdout EOF/heartbeat kill
)
```

`Supervisor` gains: negotiated-version storage, `heartbeats <-chan Heartbeat`, monotonic `lastHeartbeat` tracked by a watchdog goroutine that calls `CancelTask(reason=heartbeat_timeout)` then escalates; `Close` implements the v1.1 ordered drain (`shutdown` request → await result → `exit` notification → 2 s → SIGTERM → 3 s → SIGKILL), keeping the old escalation ladder as fallback for a host that never answers.

### 4.2 `internal/application` — `agent_ports.go` + `agent_service.go`

Same discipline as `worktree_ports.go`: one production impl per port, fakes in tests; the store owns transactions.

```go
// Ports
type AgentHostFactory interface {
    Spawn(ctx context.Context, cfg HostConfig) (AgentHost, error) // AgentHost ~= agent.Client
}
type AgentSessionStore interface {
    BeginSession(ctx context.Context, s AgentSession) (AgentSession, error) // INSERT ... unique(run_id,attempt)
    FinishSession(ctx context.Context, sessionID string, outcome SessionOutcome) error
    AppendEvents(ctx context.Context, sessionID string, evs []AgentEvent) error // batched, idempotent
    ActiveSessions(ctx context.Context) ([]AgentSession, error)
}
// Reused: WorktreeGit (HeadSHA), gitops.CanonicalPath, RegistryStore via RegistryService.

type AgentRunService struct {
    hosts    AgentHostFactory
    sessions AgentSessionStore
    registry *RegistryService
    git      WorktreeGit
    worktrees WorktreeStore     // resolve allocated path for run
    layout   gitops.Layout
    cfg      AgentRunConfig      // command, env_allowlist, timeouts, default agent_type
    clock    Clock
    logger   *slog.Logger
}

// RunCodingAttempt is THE entry point invoked by orchestration while
// run.state == 'coding'. It owns exactly one attempt, end to end.
func (s *AgentRunService) RunCodingAttempt(ctx context.Context, cmd CodingCommand) (CodingOutcome, error)

type CodingCommand struct {
    RunID, TaskID string
    Attempt       int
    LeaseOwner    string
    LeaseEpoch    int64
    Prompt        string   // scope from VTX-6 baseline
    TestBaseline  string
    TimeoutMs     int
    AgentType     string   // "pi" default; "omp" only when policy/task requests
}

type CodingOutcome struct {
    SessionID  string
    Status     string      // succeeded | failed | cancelled | infra_error
    CommitSHA  string      // captured via HeadSHA when succeeded
    Retryable  bool
    ErrorCode  string
}
```

`RunCodingAttempt` flow (each step's writes are durable before the next starts):

1. Resolve allocated worktree row → `CanonicalPath` check → `BeginSession(status='starting')`.
2. `Spawn` host (`Setpgid`, env filtered to allowlist) → `initialize` with `protocol_version:"1.1"`, `run_id`, `agent_type`; persist negotiated version + `agent_version` + `is_test_double`; status `ready`.
3. `task/execute` (`workspace_path`, `prompt`, `test_baseline`, `timeout_ms`, `attempt`); status `running`; forward `Events()` into a batching writer → `AppendEvents` (idempotent on seq); drain stderr to slog.
4. Terminal result → if `succeeded`: `git.HeadSHA(worktree)` → `CodingOutcome{CommitSHA}`; `FinishSession(status)`.
5. Apply outcome to the run via `RegistryService.TransitionRun` — **inside the lease**: success → `advance` coding→verifying with `commit_sha` set on the run row; failure → `schedule_retry`/`fail` per §6 classification; `cancelled` → the run's own `cancel`/`block` transition (operator-driven) is authoritative — a cancelled attempt never advances.
6. `Shutdown(ctx)` — ordered drain, always reaped. `defer` covers panic paths: host dies → session `killed`, run decision left to reconciler/policy, never a phantom advance.

`AgentEventWriter` (same file): bounded channel → flush every 100 events or 500 ms → `AppendEvents`; overflow drops with a `log` event marker, never blocks the host (backpressure rule: supervisor must always read).

### 4.3 `apps/agent-runtime` changes (TS side, design only)

- `adapters/pi.ts`: real `PiCliAdapter` — `spawn(PI_CMD, ["--print"|headless flags], { cwd: workspace_path, env: pick(env, allowlist), detached:false })` (children stay in host's pgid → group kill reaches them); pipes CLI stdout/stderr into `onEvent`; resolves `succeeded` on exit 0, `failed` otherwise; `signal` → SIGTERM → 3 s → SIGKILL the child. `isSimulation=false`.
- `adapters/omp.ts`: same shape, OMP CLI flags; secondary — used only when `agent_type=omp`.
- Keep `PiAgentAdapter`/`OmpAgentAdapter` doubles for tests, renamed internally to make the double status unmistakable (`test_double: true` reported).
- `host.ts`: add `host/heartbeat` emitter while a task is active; implement `exit` notification (`process.exit(0)`); honour negotiated version (v1.0 supervisor → no heartbeat); populate `adapters[].test_double` and `result.test_double`; hook `console.*` into `task/event{event_type:"log"}`.

## 5. State machine interactions (SRS §5)

```
 queued ──begin(scheduler)──▶ preparing ──advance(orchestrator)──▶ coding
                                                                   │  VTX-7: RunCodingAttempt
                          coding ──advance(orchestrator, sha set)──▶ verifying   [flip operational]
                          coding ──schedule_retry(policy)──▶ retry_wait ──retry_due──▶ resume_state=coding
                          coding ──block(operator)──▶ blocked ──resume──▶ coding
                          coding ──cancel/fail──▶ cancelled | failed (terminal)
```

| Interaction | Action / actor | Effect |
|---|---|---|
| Admission complete, worktree allocated | `advance` preparing→coding, actor `orchestrator` | Becomes operational with this spec; precondition: `worktrees.status='allocated'` + session `starting` row written. |
| `task/execute` → `succeeded` + `HeadSHA` captured | `advance` coding→verifying, actor `orchestrator`, CAS on version + lease epoch; `task_runs.commit_sha` set | Hand-off to VTX-8; hop stays `operational:false` until VTX-8 lands (FR-REG-007). |
| `failed` / `ErrHostDied` / timeout — retryable | `schedule_retry`, actor `policy` | `resume_state=coding`, `retry_due_at` per VTX-11 budget; next attempt spawns a NEW host + session (`attempt+1`). |
| `failed` non-retryable (`-32001`, `-32003`, `-32004`) or budget exhausted | `fail`, actor `policy` | Terminal `failed`; session row keeps error code. |
| Operator `block`/`cancel` | `block`/`cancel` (authorized actors) | Supervisor sends `task/cancel` reason `operator_block`/`operator_cancel` then drains host; blocked runs keep session `cancelled`. |
| Worker death mid-attempt | reconciler path | Lease expiry lets recovery mark session `crashed` (stale `running` rows + dead pid) and drive `schedule_retry`; worktree untouched (VTX-4 rule). |

`is_test_double` sessions may drive transitions in test/dev environments only; production evidence flow is identical — the flag only governs what downstream QA/gate may *accept*.

## 6. Error / retry semantics

| Outcome | Detection | Session status | Run action | Retryable |
|---|---|---|---|---|
| `status:"succeeded"` | terminal result | `succeeded` | `advance` → verifying (+commit_sha) | n/a |
| `status:"failed"` | terminal result | `failed` | `schedule_retry` (budget: VTX-11) | yes, resume `coding` |
| `status:"cancelled"` | terminal result after `task/cancel` | `cancelled` | follow run-level transition already requested (block/cancel) | per operator |
| Execute `timeout_ms` | host timer → abort → `cancelled` with `error` | `cancelled` (reason `timeout`) | `schedule_retry` | yes, bounded |
| Host stdout EOF / pid dead | read loop ends w/o terminal result | `crashed` | `schedule_retry` | yes |
| Heartbeat: 3 missed | watchdog | `running` → `task/cancel`(`heartbeat_timeout`) | wait for terminal; else ↓ | — |
| Heartbeat: 5 missed | watchdog | `killed` (SIGKILL group) | `schedule_retry` | yes |
| `-32001` version | `initialize` error | `failed`/`agent_unavailable` | `fail` | no — config fix required |
| `-32003` CLI missing | preflight probe | `agent_unavailable` | `fail` (or block pending ops fix) | no |
| `-32004` bad worktree | pre-spawn validation | `failed` | `fail` | no — supervisor bug |
| `ErrStaleEpoch`/lease lost at apply time | `TransitionRun` reject | `killed` reason `lease_lost` | none — new owner decides | n/a |
| Spawn failure (`exec` error) | `Spawn` err | `failed` | `schedule_retry` (infra) | yes |

Rules: the host never retries `task/execute` internally; one attempt one process. Retrying always means a fresh session+attempt row, preserving evidence. Escalation ladder is fixed: `task/cancel` → `shutdown`/`exit` → group SIGTERM (3 s) → group SIGKILL → `cmd.Wait()` (`WaitDelay` 10 s).

## 7. Test matrix (SRS §9 evidence)

§9 mapping: VTX-14 line ("runtime IPC smoke") and VTX-6..12 line ("real agent integration"). IDs below become test names.

| ID | Test | Evidence for |
|---|---|---|
| AGT-01 | Golden NDJSON transcript (init→execute→events→terminal→shutdown/exit) parses identically in Go supervisor and TS host | §9 VTX-14 IPC smoke; contract v1.1 |
| AGT-02 | Corrupt line mid-stream skipped; subsequent messages processed; `host/log` notice emitted | resync rule |
| AGT-03 | `initialize` major mismatch → `-32001` → supervisor kills pgid; session `failed` | FR-AGT-002 negotiation |
| AGT-04 | Negotiation down: v1.1 supervisor ↔ v1.0 host → no heartbeat expected, session proceeds | backward compat |
| AGT-05 | `task/cancel` mid-execute → terminal `cancelled` result received; process group dead (`kill(-pgid,0)` ESRCH), zero zombies | FR-AGT-002 cancellation + cleanup |
| AGT-06 | Agent CLI spawns grandchild; kill ⇒ whole tree reaped (pgid assertion) | process-group cleanup, research §5 |
| AGT-07 | Heartbeat stall (host sleeps) → cancel at 3 misses, kill at 5; session `killed` | watchdog |
| AGT-08 | `test_double` propagation: fake adapter session persists `is_test_double=true`; real adapter `false`; evidence query distinguishes | FR-AGT-003 |
| AGT-09 | Second `task/execute` same `task_id` → `-32600`; concurrent different `task_id` → `-32002` | single-task discipline |
| AGT-10 | Host killed mid-execute → session `crashed`, run → `retry_wait` via policy actor; replay-safe | NFR-REL-001, E2E-03 |
| AGT-11 | Stale lease epoch at apply → no state write, host killed `lease_lost` | FR-SCH-002 fencing |
| AGT-12 | `workspace_path` traversal (`../`, symlink) → `-32004`, session `failed`, run `failed` | NFR-SEC-001 |
| AGT-13 | Env allowlist: non-listed var absent in agent child; no secret string in `agent_events` payloads (redaction assertion) | NFR-SEC-001/AUD-001 |
| AGT-14 | Real Pi CLI smoke (feature-flagged, requires `pi` binary): executes in worktree, produces commit, `HeadSHA` persisted, `test_double=false` | §9 "real agent integration", OD-07 exit |
| AGT-15 | `agent_events` `(session_id,seq)` conflict → dedup; restart-safe append | idempotent evidence |
| AGT-16 | `preparing→coding` advance now operational; `coding→verifying` still `ErrStageNotOperational` until VTX-8 | FR-REG-007 |
| AGT-17 | Backpressure: event flood keeps host unblocked; writer drops logged, cap enforced | research §4 |

## 8. Open decisions

| ID | Decision | Status |
|---|---|---|
| D7-1 | NDJSON-over-stdio framing kept; LSP-style framing rejected | **DECIDED** — `docs/research/agent-ipc-protocol.md` ranked it; resync property and existing code both support it. |
| D7-2 | One host process per run attempt (no multiplexing, no pooling) | **DECIDED** — single-tenant keeps `seq`, cancellation, and pgid cleanup trivial; respawn cost is irrelevant at ≤3 concurrent runs (FR-SCH-001). |
| D7-3 | Commit SHA captured by supervisor (`git rev-parse HEAD`), not reported by agent | **DECIDED** — FR-QA-003 demands evidence tied to the exact SHA; an agent-reported SHA is unverifiable. |
| D7-4 | No in-runtime Pi→OMP fallback within a run; `agent_type` fixed per attempt | **DECIDED** — mixed agents per run blur evidence; fallback belongs to retry policy (VTX-11) choosing `agent_type` per attempt. |
| D7-5 | Heartbeat defaults: `heartbeat_ms=5000`, cancel at 3 misses, kill at 5 | **DECIDED** — values straight from research doc; configurable via `HostConfig`. |
| D7-6 | `shutdown`→result→`exit`→2s→SIGTERM→SIGKILL ordered drain | **DECIDED** — LSP-style handshake retained from research; old direct-kill path remains fallback. |
| D7-7 | `env_allowlist` names-only contract; values injected at spawn | **DECIDED** — prevents secret leakage through events/logs (NFR-SEC-001). |
| D7-8 | Pi CLI headless mode flags and exact binary name | **STAKEHOLDER** — depends on Pi runtime packaging (SRS OD-07); verify on real install before implementation lane starts. Owner: platform admin. |
| D7-9 | Node.js version pin for `apps/agent-runtime` in workers/containers | **STAKEHOLDER** — ops decision; affects `host.node_version` reported in initialize. Owner: platform admin. |
| D7-10 | Emit `agent_session.finished.v1` outbox event for VTX-13 live feed | **DECIDED** — recommended yes (cheap, dashboard already consumes outbox events); coordinator to include in merged events contract. |
| D7-11 | `agent_events` retention/volume cap beyond 64 KiB/event | **STAKEHOLDER** — ties to OD-06 storage/retention policy. Recommend time-based retention job; size TBD. |
| D7-12 | Prompt/scope format consumed from VTX-6 baseline | **STAKEHOLDER** — interface contract owned jointly with lane A (VTX-6); this spec passes it opaquely as `prompt`+`test_baseline`. |
