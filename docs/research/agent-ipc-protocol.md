# Agent IPC Protocol — Go Supervisor ↔ Node.js Pi/OMP Host over stdio

**Date:** 2026-10-14 | **Scope:** VTX-14 bootstrap, contract `agent-ipc/v1` (files under `contracts/agent/`)
**Target:** BS-12/13/14 — handshake, streaming, cancellation, no zombie leaks.

## 1. Recommendation (ranked)

| # | Decision | Choice | Why |
|---|----------|--------|-----|
| 1 | Wire format | **JSON-RPC 2.0 over NDJSON** (one message per `\n`-terminated line on stdout) | Trivial resync: one corrupt line is skipped, stream survives. LSP `Content-Length` framing cannot resync mid-stream and buys nothing at this scale. Precedent: MCP stdio transport uses NDJSON. |
| 2 | Channel discipline | stdout = protocol only; diagnostics → stderr; agent stdout text → `task/event` notifications | A stray `console.log` corrupts the stream; wrap console + treat non-JSON lines as `host/log` notices. |
| 3 | Lifecycle | One child process per task-lease (one agent per worktree); `initialize → execute_task → task/event* → shutdown/exit` | Matches SPEC-001 "one coding agent active per task"; keeps protocol state single-tenant. |
| 4 | Cleanup | `Setpgid` + group `SIGTERM` → 3 s grace → group `SIGKILL`; always `cmd.Wait()` | `exec.CommandContext` kills only the direct child; grandchildren (shell, pi subprocess) leak. |

## 2. JSON-RPC 2.0 schema (`contracts/agent/v1/`)

Envelope: `{"jsonrpc":"2.0", ...}` — requests carry `id` (uint64 from supervisor); notifications omit `id`.

### 2.1 `initialize` (request, first message, 10 s deadline)
```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
  "protocol_version":"1.0","supervisor":{"name":"vtx-agent-adapter","version":"0.1.0"},
  "capabilities":{"cancellation":true,"streaming":true,"heartbeat_ms":5000}}}
```
Response:
```json
{"jsonrpc":"2.0","id":1,"result":{
  "protocol_version":"1.0","host":{"runtime":"pi","version":"x.y.z","node":"v20.x"},
  "capabilities":{"tools":["read","write","bash"],"max_event_bytes":65536}}}
```
Version mismatch → `error:{code:-32001,"message":"unsupported protocol_version"}` then supervisor kills the process.

### 2.2 `task/execute` (request; result returned only at terminal state)
```json
{"jsonrpc":"2.0","id":2,"method":"task/execute","params":{
  "task_id":"VTX-14-001","worktree":"/ws/tasks/VTX-14-001",
  "prompt":"...","timeout_ms":1800000,"env_allowlist":["PATH","HOME"]}}
```
Terminal result: `{"jsonrpc":"2.0","id":2,"result":{"status":"succeeded","summary":"...","artifacts":[...]}}`
`status ∈ succeeded | failed | cancelled`. Failure detail goes in `error` only for protocol faults; task failure is `status:"failed"` + `task/event{kind:"error"}` — keeps the stream parseable.

### 2.3 `task/event` (notification, host → supervisor, streaming)
```json
{"jsonrpc":"2.0","method":"task/event","params":{
  "task_id":"VTX-14-001","seq":41,"ts":"2026-10-14T10:00:01.234Z",
  "kind":"stdout|stderr|tool_call|file_diff|progress|log","data":{...}}}
```
`seq` is per-task, gap-checkable by supervisor (missing seq ⇒ warn, resync not required in v1).

### 2.4 `task/cancel` (notification, supervisor → host)
```json
{"jsonrpc":"2.0","method":"task/cancel","params":{"task_id":"VTX-14-001","reason":"timeout|user|lease_lost"}}
```
Host aborts via `AbortController`; must still send the terminal `task/execute` result (`status:"cancelled"`).

### 2.5 `host/heartbeat` (notification, every `heartbeat_ms`)
```json
{"jsonrpc":"2.0","method":"host/heartbeat","params":{"task_id":"VTX-14-001","pid":1234}}
```
Supervisor: 3 missed beats ⇒ log + `task/cancel`; 5 ⇒ group SIGKILL.

### 2.6 `shutdown` / `exit` (LSP pattern)
`shutdown` request → host finishes pending writes, replies `{}` → supervisor sends `exit` notification → host `process.exit(0)` within 2 s else SIGKILL.
Error codes: standard JSON-RPC (`-32700` parse, `-32600` invalid, `-32601` unknown method); custom `-32000` task_not_found, `-32001` version, `-32002` busy (host rejects second `task/execute`).

## 3. Go process management — kill the group, not the child
```go
func Spawn(ctx context.Context, bin string, args ...string) (*Host, error) {
	cmd := exec.Command(bin, args...) // NOT CommandContext: we escalate ourselves
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own pgid
	stdout, _ := cmd.StdoutPipe(); stderr, _ := cmd.StderrPipe()
	stdin,  _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil { return nil, err }
	pgid := cmd.Process.Pid // leader ⇒ pgid == pid
	go scanNDJSON(stdout, events) // MUST drain: full pipe blocks host writes
	go scanLines(stderr, logs)
	h := &Host{cmd: cmd, stdin: stdin, pgid: pgid, done: make(chan error, 1)}
	go func() { h.done <- cmd.Wait() }() // reaps zombie; also releases pipes
	return h, nil
}

func (h *Host) Terminate(ctx context.Context) error {
	syscall.Kill(-h.pgid, syscall.SIGTERM) // negative pid ⇒ whole group
	select {
	case err := <-h.done: return err        // host exited
	case <-time.After(3 * time.Second):     // grace period
	case <-ctx.Done():                      // supervisor is shutting down too
	}
	syscall.Kill(-h.pgid, syscall.SIGKILL)
	return <-h.done // always reap; guarantees no zombie even if group kill raced
}
```
Critical caveats:
- `cmd.Wait()` can hang after kill if grandchildren inherited the stdout pipe — the drain goroutine + `Setpgid` kill fixes this; add `cmd.WaitDelay = 10s` (Go ≥1.20) as belt-and-suspenders.
- In the Docker sandbox, run the supervisor with `tini`/`--init` so orphaned grandchildren get reaped by PID 1.
- Linux-only `Setpgid` for Phase 0; put behind `spawn_linux.go` build tag.

## 4. Node.js host framing
```ts
// Inbound: newline-delimited JSON on stdin
const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
rl.on("line", l => { const msg = JSON.parse(l); dispatch(msg); });
// Outbound: serialize writes; never interleave bytes
function send(msg: object) { process.stdout.write(JSON.stringify(msg) + "\n"); }
```
Rules: hook `console.*` → `task/event{kind:"log"}`; backpressure via `write()` return value → pause event source, buffer with cap (64 KiB), overflow events coalesce into `kind:"progress"`; all Pi/OMP subprocess output captured via `child_process` events, never piped to stdout.

## 5. Risks & mitigations
- **stdout pollution** (agent/lib prints raw) → non-JSON line ⇒ `host/log` notice on supervisor, CI conformance test BS-12 fails the build. Mitigation: console hooks + document "stdout is protocol".
- **Pipe backpressure deadlock** (supervisor slow to read) → undrained stdout blocks host mid-event. Mitigation: dedicated drain goroutine, `WaitDelay`, bounded event size.
- **Grandchild leak** (pi spawns `git`, `npm`…) → killed with group; add `tini` in container.
- **Half-written line on kill** → NDJSON resync is free: partial tail is one discarded line.
- **Version skew** Pi vs OMP hosts → negotiated `protocol_version`, JSON Schema files in `contracts/agent/v1/` validated by CI conformance suite.

## 6. Out of scope / limitations
Windows process-group semantics (Phase 0 is Linux+Docker only); task resume/reconnect after supervisor crash (v2, needs persisted `seq`); multiplexed tasks per host (explicitly rejected — one lease per process).
