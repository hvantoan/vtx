# VTX Orchestrator — Pi-Native Agent Orchestration (Extension) — Design Spec

**Epic:** VTX-ORCH (work package: AI orchestrator agent that plans from a clear spec and delegates to specialist agents)
**Status:** Design spec for review; supersedes the fixed `RunExecutionWorkflow` Temporal chain proposed earlier for the `coding → verifying → qa → quality_gate → pr_draft` driver.
**Decision:** Phương án **A1** — Pi-native orchestration (goals/subagents) is the driver; a single VTX Pi **extension** is the bridge + guardrail; the Go spine stays the durable source of truth and the authority for gate/lease/admission/evidence.
**Grounding:**
- Pi extension API: `@earendil-works/pi-coding-agent` v1.4.0 — `docs/extensions.md`, `dist/core/extensions/types.d.ts` (`ExtensionAPI`, `ExtensionContext`, `ExtensionToolContext.executeTool`, `ToolDefinition`), `examples/extensions/subagent/` (native delegation), `examples/extensions/permission-gate.ts` (fail-closed gating).
- Live extension precedents in this environment: `~/.pi/agent/extensions/herdr-agent-state.ts` (socket reporting to an external supervisor), `compaction-policy.ts`, `orca-prefill.ts`.
- VTX existing seams (reused, not rewritten): `apps/backend/internal/agent/supervisor.go`; `apps/agent-runtime/src/{host.ts,adapters/{cli,pi,omp}.ts,protocol/types.ts}`; `internal/application/{verification_service,pr_service,registry_service,worktree_service}.go`; `internal/qa/service.go`; `internal/gate/policy.go`; `internal/scheduler/scheduler.go`; `internal/domain/registry.go`.
- SRS §4 (FR-*), §5 state model, §7 Agent IPC, §9 evidence.

---

## 1. Overview & Motivation

The implemented VTX pipeline stops being automatic after `preparing`. The Go state machine defines `coding → verifying → qa → quality_gate → pr_draft → completed` ([`internal/domain/registry.go`](../../apps/backend/internal/domain/registry.go)), and every downstream service exists with a clean seam (`VerificationService.VerifyRun`, `qa.Service.LaunchSession/SubmitVerdict`, `gate.Evaluate`, `PRService.OpenDraftPR`). But **no component drives the chain**:

- No workflow/activity transitions a run past `coding` (no `ExecutionWorkflow` exists).
- `qa.Service.Runner` (`AgentRunner`) is never assigned, so QA fails closed ([`internal/qa/service.go`](../../apps/backend/internal/qa/service.go)).
- `WorktreeActivities.RenewWorktreeLease` has no caller, so a long coding run loses its lease ([`internal/orchestration/worktree.go`](../../apps/backend/internal/orchestration/worktree.go)).

Rather than build a new deterministic Temporal driver, **A1 makes Pi's own orchestration the driver.** Pi v1.4.0 already ships a delegation mechanism (the `subagent` example: specialist agents as `.md` configs, a tool that spawns isolated `pi` subprocesses in single/parallel/chain modes) and a lifecycle-hook extension system. A VTX extension wires that native orchestration into the existing Go services and enforces VTX's safety invariants from inside the agent loop.

**Guiding principle — "AI decides, deterministic rails enforce."** The orchestrator agent chooses *which* tool to call and *with what arguments*; it can never forge evidence, self-approve a gate, bypass QA independence, or exceed admission/budget. Those remain owned by the Go spine and re-checked by fail-closed extension hooks.

### Goals
- Drive a run end-to-end (`preparing → … → completed`) using Pi-native planning + delegation.
- Reuse existing Go services unchanged; add only a thin tool-server that exposes them.
- Preserve every VTX invariant (gate not LLM-alone, QA independence + exact SHA, lease fencing, admission ≤3, bounded retry, linked audit).
- Operate **headless** — VTX runs Pi in `--mode json` (no TTY/UI).

### Non-goals
- Auto-merge or deploy (FR-PR-001 keeps Draft-PR-only).
- Replacing the scheduler/admission, worktree lifecycle, or outbox/DLQ subsystems — A1 consumes them.
- Building a new agent framework — Pi is the framework.
- Backward retry edges (see D-11.2 in [`vtx-11-retry-orchestration.md`](vtx-11-retry-orchestration.md)); the state machine stays a DAG.

---

## 2. Architecture

```
                        ┌──────────────────────────────────────────────┐
   clear spec  ───────▶ │  ORCHESTRATOR Pi session  (--mode json)       │
   (Jira/VTX-6)         │  native goals/tasks + `subagent` delegation    │
                        │  loads: vtx-orchestrator.ts (extension)        │
                        └───┬───────────────────────────┬───────────────┘
             delegate       │                           │  tool calls (native,
        (spawn pi subproc)  │                           │  typed) + evidence sync
                            ▼                           ▼
        ┌───────────────────────────┐     ┌──────────────────────────────────┐
        │ Specialist agents         │     │  GO SPINE TOOL SERVER (unix sock) │
        │ .pi/agents/*.md           │     │  thin wrapper over EXISTING svcs: │
        │  • vtx-coder  (worktree)  │     │   VerifyRun · qa.Launch/Submit    │
        │  • vtx-qa     (read-only) │     │   gate.Evaluate · OpenDraftPR     │
        │  • vtx-reviewer           │     │   RegistryService.TransitionRun   │
        │ (isolated ctx per run)    │     │   WorktreeService.RenewLease      │
        └───────────────────────────┘     └──────────────┬───────────────────┘
                                                          │ authoritative writes
                                                          ▼
                                          PostgreSQL (registry, evidence, outbox,
                                          lease/admission) ── SSE ──▶ dashboard
```

Two channels, deliberately separate:
1. **Delegation channel** — orchestrator → specialist `pi` subprocesses (Pi-native `subagent` tool; isolated context windows; worktree-scoped cwd).
2. **Authority channel** — orchestrator extension → Go spine tool server (unix socket) → existing services → Postgres. All state transitions, gate decisions, lease renewals, PR creation flow here and **only** here.

The orchestrator never talks to Postgres or GitHub directly. It has no credentials for them; the spine does.

---

## 3. Component Design

### 3.1 Specialist agents (A1 delegation) — `.pi/agents/*.md`

Project-local agent definitions, discovered by Pi's agent loader (frontmatter `name`, `description`, `tools`, `model`, `systemPrompt`), delegated to via the `subagent` tool's single/parallel/chain modes.

| Agent | Profile | Tools (allowlist) | Role |
|---|---|---|---|
| `vtx-coder` | coding | read, edit, write, bash, grep, find, ls | Implements the spec inside the run worktree; commits. Maps to existing `AgentWorkSpec.Profile="coding"`. |
| `vtx-qa` | qa (read-only) | read, grep, find, ls, bash(test-only) | Independently evaluates the **exact commit SHA** + test evidence; MUST NOT commit/push. Maps to `Profile="qa"`, `ReadOnly=true`. |
| `vtx-reviewer` | review | read, grep, find | Optional static/security review feeding gate evidence. |

Delegation contract (chain preset in `.pi/prompts/vtx-run.md`):
`vtx-coder → (commit SHA) → vtx-qa → verdict → gate → open_pr`.
Concurrency is capped by the spine's `MaxAdmissionSlots = 3` ([`internal/scheduler/scheduler.go`](../../apps/backend/internal/scheduler/scheduler.go)), not by the agent; the `subagent` tool's own `MAX_CONCURRENCY` is set to match so the agent cannot outrun admission.

**QA independence** is enforced twice: (a) `vtx-qa` runs in a separate `pi` process with its own session id; (b) `qa.Service.LaunchSession` already rejects a QA session equal to the coding session (`ErrQASessionNotUnique`) and a SHA mismatch (`ErrQASHAMismatch`) ([`internal/qa/service.go`](../../apps/backend/internal/qa/service.go)).

### 3.2 `vtx-orchestrator.ts` extension

Loaded into the orchestrator Pi session (`--extension` flag or `~/.pi/agent/extensions/`). Headless-safe: every UI path guarded by `ctx.hasUI`/`ctx.mode`; **no `ctx.ui.confirm`** because JSON mode has no UI (see §6). All safety decisions fail closed.

**Registered tools** (`pi.registerTool`, typed `parameters` via TypeBox, `outputSchema` + `structuredContent` for evidence). Each tool is a thin RPC to the spine; the *service* decides, not the agent.

| Tool | exposure | annotations | Backing spine call | Rail |
|---|---|---|---|---|
| `vtx_plan` | `direct` | readOnly | `RegistryService` metadata + outbox event | persists task DAG; emits SSE |
| — (native `subagent`) | `model-only` (Pi) | — | pi-subagents foreground child | `tool_call` gate: agent allowlist, delegation budget, lease, cwd/scope pinned; `workflow`/`chain`/`tasks`/`async`/`model` refused |
| `vtx_verify` | `direct` | readOnly | `VerificationService.VerifyRun` | evidence row required |
| `vtx_qa` | `direct` | readOnly | `qa.LaunchSession`/`SubmitVerdict` | QA≠coding, exact SHA |
| `vtx_gate` | `direct` | readOnly | `gate.Evaluate` | **reads DB evidence; agent input cannot set PASS** |
| `vtx_open_pr` | `direct` | `destructiveHint` | `PRService.OpenDraftPR` | refuses unless gate PASS (already enforced in `OpenDraftPR`) |
| `vtx_advance` | `direct` | — | `RegistryService.TransitionRun` | version CAS + lease epoch fence |
| `vtx_block` / `vtx_escalate` | `direct` | — | `TransitionRun(block)` + operator event | audit actor/reason/correlation |

`vtx_gate` and `vtx_open_pr` take **no verdict argument** — they pass `run_id` and the spine recomputes the decision from committed evidence. This is the concrete realization of FR-QA-002 (gate not LLM-alone).

**Lifecycle hooks** (`pi.on`):

| Hook | Purpose | Fail behavior |
|---|---|---|
| `session_start` | Open socket to spine, register run, fetch spec/context; start heartbeat → `RenewLease` | If spine unreachable → do not proceed (fail closed) |
| `session_shutdown` | Idempotent socket close, final evidence flush, `RenewLease` stop | Must be idempotent (cancel/reload/exit converge) |
| `before_agent_start` | Inject the clear spec + acceptance criteria into `systemPromptOptions`; set active tools for the current stage | Preserve fields not owned |
| `tool_call` | **Authorize/gate every tool**: allowlist, budget, lease-held precondition, spine reachability. Mutate the native `subagent` input to pin cwd/agent scope and force foreground execution | Block (`{block:true, reason}`) on any failed check; handler failure blocks the tool (Pi fail-safe) |
| `turn_end` / `agent_before_settle` | Budget & stop-condition guard; cap continuations to prevent runaway loops | Stop continuing when budget/stop hit; emit `vtx_block` |
| `tool_result` | Redact secrets from evidence before persist/sync | — |

Long-lived resources (socket, heartbeat timer) are started from `session_start`, **never** in the factory (Pi loads extensions without starting a session in some invocations — `docs/extensions.md`).

**State:** durable, non-context orchestration state (task DAG, budget counters, delegation ids) via `pi.appendEntry()`; branch-sensitive state reconstructed from `ctx.sessionManager.getBranch()` on `session_start`. Evidence destined for the dashboard is returned as tool `structuredContent` and pushed to the spine, which owns persistence.

### 3.3 Go spine tool server (thin)

New internal server exposing existing services over a unix socket (JSON lines), consumed only by the extension. It adds **no business logic** — it authenticates the caller, decodes the request, calls the service, returns the structured result. Reuses `AgentWorkSpec`-style auth token pattern already present (`QA_AGENT_AUTH_TOKEN`).

```
internal/orchestration/spineserver/   (new)
  server.go   — unix-socket listener, request routing, auth, redaction
  tools.go    — one handler per vtx_* tool → existing service call
cmd/worker/spine.go (new)  — `worker spine` process mode (Postgres-only; no Temporal)
```

Handlers map 1:1 to services already constructed in `cmd/api/main.go` (`WithQuality`, `WithPullRequests`) — the same wiring, exposed on a socket instead of HTTP. Where a service needs a lease epoch/version, the extension passes what the spine gave it at `session_start`; the service re-validates via existing CAS.

**Process isolation:** the spine server runs as a `worker spine` mode (Postgres-only), consistent with the existing "one process boundary per mode" rule (D8/TR-08) and independent of Temporal availability.

### 3.4 Spec contract (the "clear spec")

Machine-readable, produced by VTX-6 `requirement.Service` (already extracts requirement + acceptance criteria from Jira) and injected via `before_agent_start`. Shape:

```yaml
run_id: <uuid>
goal: <one-line objective>
acceptance_criteria:            # → QA verifies these against exact SHA
  - <criterion>
repo: { url, base_ref, worktree_path }
test_baseline: <command(s) + expected>
allowed_tools: [vtx_plan, vtx_verify, vtx_qa, vtx_gate, vtx_open_pr, vtx_advance, vtx_block]
# delegation uses Pi's native `subagent` tool; the gate pins its cwd and args
delegation:
  agents: [vtx-coder, vtx-qa, vtx-reviewer]
  max_parallel: 3               # == MaxAdmissionSlots
  qa_independent: true          # invariant, not negotiable by the spec
budget: { max_turns: N, max_tokens: M, wall_clock_ms: T, max_delegations: K }
stop_conditions: [gate_pass_and_pr_open, budget_exhausted, blocked, no_progress]
lease: { owner, epoch, expires_at, renew_interval_ms }
```

Spec content originating from Jira is **untrusted input** (§7).

### 3.5 Transport / protocol

- Unix domain socket (path via env, mirroring the herdr precedent `HERDR_SOCKET_PATH`), newline-delimited JSON.
- Request: `{ id, tool, run_id, lease_epoch, args }`. Response: `{ id, ok, structuredContent | error }`.
- Extension → spine only; the spine never initiates. Heartbeat is an extension-side timer calling `RenewLease`; socket EOF or heartbeat loss ⇒ spine marks the session crashed and the run goes `retry_wait`/`failed` via policy (never silently `completed`).

---

## 4. Mapping to Pi ExtensionAPI (grounded)

| VTX need | Pi v1.4.0 symbol | Source |
|---|---|---|
| Register native orchestration tools | `ExtensionAPI.registerTool(ToolDefinition)` | `types.d.ts:1229`, `:453` |
| Typed params / structured evidence | `ToolDefinition.parameters` (TypeBox), `outputSchema`, result `structuredContent` | `types.d.ts:465,476,997` |
| Orchestrating tool that delegates | `exposure:"model-only"`, `reachesTools`, `prepareLoadout`, `ExtensionToolContext.executeTool` | `types.d.ts:480,490,504,280`; `extensions.md` §Tools |
| Native subagent delegation | `examples/extensions/subagent` (spawns isolated `pi`, single/parallel/chain) | `examples/extensions/subagent/index.ts` |
| Specialist agent configs | `.pi/agents/*.md` frontmatter (`discoverAgents`, `getAgentDir`, `parseFrontmatter`) | `examples/extensions/subagent/agents.ts` |
| Inject spec / stage tools | `pi.on("before_agent_start")` → `systemPromptOptions`; `pi.setActiveTools()` | `extensions.md` §events; `types.d.ts:1291` |
| Fail-closed authorization | `pi.on("tool_call")` → `{block, reason}`; handler failure blocks | `examples/extensions/permission-gate.ts`; `extensions.md` |
| Budget / stop guard | `pi.on("turn_end"|"agent_before_settle")` continuation guard | `extensions.md` §events |
| Spine socket + cleanup | `pi.on("session_start"|"session_shutdown")`; `ctx.signal`, `ctx.shutdown()` | `herdr-agent-state.ts`; `types.d.ts:213` |
| Durable orchestration state | `pi.appendEntry()`, `ctx.sessionManager.getBranch()` | `types.d.ts:1272`; `extensions.md` §State |
| Lease heartbeat | extension timer → `vtx_advance`/`RenewLease` RPC | (new) |
| Secret redaction | `pi.on("tool_result")` redact `content` + `structuredContent` | `extensions.md` §events |

---

## 5. Invariant / SRS coverage

| ID | Requirement | How A1 preserves it |
|---|---|---|
| FR-QA-001 | QA independent from coding agent, evaluates exact SHA + test evidence | Separate `vtx-qa` subprocess + `qa.LaunchSession` `ErrQASessionNotUnique`/`ErrQASHAMismatch` (unchanged) |
| FR-QA-002 | Gate PASS only on mandatory conditions, **not LLM-alone** | `vtx_gate` takes no verdict arg; `gate.Evaluate` reads committed evidence; extension cannot forge |
| FR-QA-003 | SHA change after QA invalidates evidence | `qa.Service` exact-SHA check unchanged; `vtx_open_pr` re-reads gate state |
| FR-PR-001 | Draft PR only after gate PASS; no auto-merge/deploy | `PRService.OpenDraftPR` "refuses before any GitHub call" unless PASS (unchanged) |
| FR-SCH-001/002 | Admission ≤3; lease owner+epoch fencing | `MaxAdmissionSlots` in spine; every mutating RPC carries `lease_epoch`, CAS re-validates; stale ⇒ discard + kill session |
| FR-SCH-003/004 | Per-run worktree isolation; crash recovery never removes leased tree | native `subagent` cwd pinned to allocated worktree via `tool_call` mutation; reconcile stays VTX-4 |
| FR-AGT-001/002/003 | Agent scope+baseline, handshake/cancel/stream, labeled test-double | Reuses VTX-7 supervisor + adapters; simulation stays `test_double=true` and cannot satisfy real-agent acceptance |
| FR-ERR-001 | Bounded retry, terminal failure + evidence | Zero in-band retry per attempt; failures → `schedule_retry`/`fail` via spine; retry budget owned by VTX-11 |
| FR-UI-003/004 | Operator mutations authenticated + audited; SSE not query-token | Unchanged; orchestrator uses system actor for transitions, operator paths untouched |
| NFR-AUD-001 | Linked audit issue→intent→run→SHA→PR; no secrets | Evidence synced through spine (single writer); `tool_result` redaction; env allowlist only names |
| NFR-REL-001 | No lost state on crash; replay safe | Socket EOF/heartbeat loss ⇒ crashed session, policy transition; `appendEntry` durable |
| NFR-SEC-001 | Least privilege, fail-closed | Extension holds no DB/GitHub creds; spine authenticates caller; all gates fail closed |

---

## 6. Headless constraint (critical)

VTX invokes Pi via `pi --mode json -p --no-session <prompt>` ([`apps/agent-runtime/src/adapters/pi.ts`](../../apps/agent-runtime/src/adapters/pi.ts)). In JSON/print mode `ctx.hasUI === false` and there is **no** `ctx.ui.confirm`. Consequences:

- The orchestrator session must drop `--no-session` (it needs a persistent session for the task DAG) — a new adapter/profile, not the specialist coding adapter.
- All authorization is programmatic + fail-closed; the `permission-gate.ts` UI-confirm pattern is **not** usable for the orchestrator. Dangerous tools (`vtx_open_pr`) are gated by spine state, not by a human prompt (operators act through the existing dashboard/operator API).
- The native `subagent` child keeps `--mode json` (specialists are headless workers) and does not load the orchestrator extension, so it never reaches the spine. Pi 0.83 registers `subagent` with `model-only` exposure, which `ctx.executeTool()` cannot call, so a wrapper tool is impossible: the model calls `subagent` directly and the `tool_call` gate rewrites it.

---

## 7. Security & trust

- **Extension = full OS permissions in-process** (`docs/extensions.md`): load `vtx-orchestrator.ts` only from the repo; it is part of the trusted codebase, not user-supplied.
- **Project-local agents are repo-controlled prompts** (`subagent/README.md` security model): `.pi/agents/*.md` can instruct read/bash. They live in-repo under review; tool allowlists are minimal per profile (`vtx-qa` read-only).
- **Untrusted spec/Jira content ⇒ prompt injection**: acceptance criteria and issue text are data, not instructions. Mitigations: fixed tool allowlist, `tool_call` gate, spine-side authorization, no credentials in the agent env, gate/PR decisions recomputed from evidence (an injected "approve" cannot pass).
- **Secrets**: agent child env limited to allowlisted names; values never serialized (contract rule 5); `tool_result` redaction before persistence.
- **Known exposure to remediate:** `.pi/.mcp.json` currently holds a plaintext Jira API token (gitignored, not committed). Move to env/secret store and rotate. Tracked as OD-ORCH-05.

---

## 8. Failure modes & guardrails

| Failure | Detection | Response |
|---|---|---|
| Runaway delegation/loop | `turn_end`/`agent_before_settle` budget guard (turns/tokens/wall-clock/delegations) | Stop continuing; `vtx_block` + escalate |
| Spine unreachable | socket connect/EOF, RPC error | Fail closed: block tool; mark session crashed; policy → `retry_wait`/`failed` |
| Lease lost mid-run | `RenewLease` returns `ErrStaleEpoch`/`ErrWorktreeLeaseLost` | Discard attempt, kill session group (`lease_lost`), no side effects applied |
| QA session == coding session | `ErrQASessionNotUnique` | Reject; re-delegate to fresh QA subprocess |
| SHA drift after QA | `ErrQASHAMismatch` | Invalidate evidence; gate cannot PASS |
| Agent claims success without commit | spine captures SHA via `gitops.Git.HeadSHA` (never trusts self-report) | No SHA ⇒ no advance; retry/fail |

---

## 9. Rollout phases

- **Phase 0 — Grounding (this doc).** Confirm A1, ExtensionAPI signatures, spine seams. *(done)*
- **Phase 1 — Spine tool server.** `internal/orchestration/spineserver` + `worker spine` mode exposing existing services over a unix socket; auth + redaction; unit tests per handler. No agent yet — drive via a test client.
- **Phase 2 — Extension skeleton.** `vtx-orchestrator.ts`: `session_start`/`session_shutdown` socket, `registerTool` for all `vtx_*`, `tool_call` fail-closed gate, `before_agent_start` spec injection. Headless (`--mode json`) verified.
- **Phase 3 — Specialist agents + delegation.** `.pi/agents/{vtx-coder,vtx-qa,vtx-reviewer}.md`, `.pi/prompts/vtx-run.md`; wire the `tool_call` gate to the native `subagent` mechanism; lease heartbeat.
- **Phase 4 — Budget/guardrails + evidence→SSE→dashboard.** Stop conditions, redaction, orchestrator plan/delegation surfaced on the VTX-13 dashboard.
- **Phase 5 — E2E evidence.** E2E-01/03/04/05/06/07 (SRS §9.2) against a real `pi` orchestrator; anything not verifiable is reported BLOCKED/NOT VERIFIED, never auto-PASS.

Each phase ends with `make test-boundaries vet typecheck test build` clean (AGENTS.md §3).

---

## 10. Open decisions

| ID | Question | Recommendation |
|---|---|---|
| OD-ORCH-01 | Orchestrator transport: unix socket vs internal HTTP | **Unix socket** (herdr precedent, no port/egress, simplest auth via socket perms + token) |
| OD-ORCH-02 | Delegation mechanism: Pi `subagent` (spawn `pi`) vs `ctx.executeTool` in-process | **`subagent`** — isolated context windows match per-run isolation; in-process shares the orchestrator's window |
| OD-ORCH-03 | Where the orchestrator session runs: new `worker orch` mode vs reuse agent-runtime host | Reuse **agent-runtime host** with a new `orchestrator` adapter/profile (drops `--no-session`) |
| OD-ORCH-04 | Model routing: single model vs orchestrator≠specialist models | Optional `pi.registerVirtualModel`/per-agent `model:` frontmatter; decide after Phase 3 cost data |
| OD-ORCH-05 | Jira token in `.pi/.mcp.json` | Move to env/secret store; **rotate** the exposed token |
| OD-ORCH-06 | Does the orchestrator write code itself or only delegate | **Only delegate** (A1): orchestrator plans/gates; `vtx-coder` writes. Keeps cost/latency and audit clean |

---

## 11. Verification plan

- **Unit (Go):** each spine tool-server handler against the existing service with a fake socket client; auth rejection; redaction; lease-epoch CAS rejection.
- **Unit (TS):** extension `tool_call` gate (allowlist, budget, fail-closed on spine error), `before_agent_start` spec injection, headless guard (`ctx.hasUI===false`).
- **Integration:** orchestrator session drives a seeded run `preparing → completed` with a real `pi` (or labeled test-double when a real CLI is unavailable — reported NOT VERIFIED per FR-AGT-003).
- **Invariant tests:** gate cannot PASS without evidence; QA≠coding; SHA drift invalidates; admission ≤3 under parallel delegation; lease loss fences side effects.
- **E2E:** SRS §9.2 E2E-01/03/04/05/06/07 with evidence pinned to commit SHA.
