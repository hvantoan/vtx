# VTX-8 — Execution / Build Adapters — Design Spec

**Epic:** VTX-8 (work package: Execution/build adapters)
**Status:** Design spec for implementation; related contract delta in [`contract-drafts/vtx7-8-agent-ipc.md`](contract-drafts/vtx7-8-agent-ipc.md)
**Grounding:** SRS §4.5 (FR-AGT-001, FR-QA-002/003, FR-ERR-001), §5 state model, §9 evidence; `apps/backend/internal/gitops/*.go`; `apps/backend/internal/application/{worktree,registry}_*.go`; root `Makefile` targets.

---

## 1. Overview

VTX-8 supplies the **verification stage** of the run pipeline: after the coding agent commits inside the run worktree (VTX-7), execution/build adapters run the repository's own verification commands — for this monorepo, the `make` contract `test-boundaries → vet → typecheck → test → build` — **inside the run worktree**, and persist every result as durable **run evidence keyed to the exact commit SHA** evaluated.

Core properties:

1. **Exact-SHA evidence.** Every verification row records the `HEAD` SHA at suite start. If the SHA drifts after (agent amends, human push), existing rows go stale — they never satisfy the gate (FR-QA-003 mechanism).
2. **Deterministic command set.** Suites are a fixed allowlist of make targets run via `make -C <worktree>`, never shell-interpolated agent or operator input.
3. **Append-only, idempotent.** One row per `(run_id, attempt, suite)`; a retried verification is a new attempt — evidence is never deleted or rewritten.
4. **Failure classification.** A failing test suite is a different beast from a crashed runner: `failed` sends the run back to `coding` for the agent to fix; `error`/`timeout` may re-run `verifying` — the distinction drives `resume_state`.

Non-goals: the quality-gate decision itself (VTX-10 consumes `verification_runs`), retry budget (VTX-11), QA agent (VTX-9), artifact/log object storage beyond a filesystem sink (OD-06).

## 2. SRS coverage

| SRS ID | Requirement | Design decision |
|---|---|---|
| FR-AGT-001 | Coding agent output verified (artifacts/commit SHA/logs) | `VerificationService` resolves the run's allocated worktree and `task_runs.commit_sha`, asserts `git rev-parse HEAD == commit_sha` before running, and executes the fixed make suite. Every suite writes a `verification_runs` row with command, exit code, duration, log reference+digest — the run's verification evidence. |
| FR-QA-002 | Gate on objective evidence, not LLM opinion | This epic **produces** the objective substrate: deterministic exit codes + logs per suite at a pinned SHA. VTX-10 reads these rows; no LLM text feeds the gate input we generate. |
| FR-QA-003 | SHA change after QA/verification invalidates old evidence | `verification_runs.commit_sha` is immutable per row. A `IsEvidenceCurrent(run_id, head_sha)` query answers "does a full passing suite set exist **at this SHA**". Drift detected pre-run → refuse + mark; drift detected by gate later → stale rows simply fail the lookup. New `verification_runs.status='superseded'` is written (as a marking row for the prior attempt) so the audit shows *why* evidence stopped counting. |
| FR-ERR-001 | Bounded retry, terminal failure, evidence | Per-suite timeout + whole-stage timeout bound execution. Classification (§6) maps to `schedule_retry`/`fail` via `TransitionRun`; the retry *budget* is VTX-11's. Each retried verification = `attempt+1` row set — history retained. |
| FR-REG-006/007 | States; no fake downstream success | `coding→verifying` flips `operational:true` with this epic; `verifying→qa` stays `operational:false` until VTX-9. The service calls `TransitionRun` (actor `orchestrator`, CAS + lease fence) — never writes `task_runs` directly. |
| FR-REG-005 | CAS + lease fencing on transitions | Verification runs only while holding the run lease; a lost-lease `TransitionRun` rejection aborts remaining suites and kills in-flight processes — a stale worker leaves evidence rows marked `error`/`aborted`, never `passed`. |
| NFR-MAINT-001 | Path-aware CI / conformance at exact SHA | Verification executes the repo's own `Makefile` targets — identical to the CI contract (AGENTS.md verification protocol). Evidence rows pin the SHA, satisfying "conformance at exact SHA". |
| NFR-AUD-001 | Linked audit chain | Rows join `run_id → attempt → commit_sha → suite → log`; `log_digest` (SHA-256 of captured output) makes tampering detectable; no secrets — env is a fixed minimal allowlist and command strings are constants. |
| NFR-REL-001 | No lost state on crash; replay safe | Crash mid-suite: last row stays `running` (never `passed`); reconciler marks `error` + `crash` and the stage re-runs as a new attempt. `UNIQUE(run_id, attempt, suite)` makes suite-level retry idempotent. |
| NFR-SEC-001 | Least privilege, no injection | Commands are a compile-time allowlist (`make` target names); no user/agent string ever reaches `exec`. Process runs as worker user with env allowlist; worktree path canonicalized via `gitops.CanonicalPath`. Timeouts bound resource burn. |
| NFR-OPS-001 | Observable dependencies/runs | `verification_runs.status` + durations feed VTX-13; slog carries `run_id`, `suite`, `attempt`, `sha`, `exit_code`, `duration_ms`. |
| NFR-CON-001 | Concurrent writer CAS | Transitively satisfied — verdict writes ride on `TransitionRun` CAS; evidence rows are insert-only. |
| FR-SCH-002/003 | Lease + worktree isolation | Runs inside the run's allocated worktree only; path resolved from `worktrees` row and canonicalized — two runs can never share a verification cwd. |
| NFR-PERF-001 / NFR-AVAIL-001 | TBD | Measurement points only: per-suite `duration_ms`, queue-to-start latency. |

## 3. Data model / DDL deltas (future `00008_verification.sql`)

```sql
-- +goose Up
-- +goose StatementBegin
-- VTX-8: one row per verification suite execution, keyed to the EXACT commit
-- SHA evaluated. Append-only: retries insert a new attempt; nothing updates a
-- terminal verdict — only 'running' rows may transition to a terminal status.
CREATE TABLE IF NOT EXISTS verification_runs (
    -- kind reconciled with vtx-6 §3: baseline rows run on base_sha before the
    -- agent; verification rows run on the agent commit.
    kind TEXT NOT NULL DEFAULT 'verification' CHECK (kind IN ('baseline', 'verification')),
    verification_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      UUID NOT NULL REFERENCES task_runs(run_id) ON DELETE CASCADE,
    attempt     INT  NOT NULL,                    -- verification attempt (>=1), independent of agent attempt
    suite       TEXT NOT NULL,                    -- 'make-test-boundaries' | 'make-vet' | ...
    command     TEXT NOT NULL,                    -- exact argv rendered, e.g. 'make -C <path> test'
    subject_commit_sha  TEXT NOT NULL,                    -- HEAD at suite start — FR-QA-003 anchor
    status      TEXT NOT NULL CHECK (status IN (
        'running','passed','failed','error','timeout','skipped','superseded')),
    exit_code   INT,
    duration_ms INT,
    log_ref     TEXT,                             -- artifact URI or 'inline'
    log_tail    TEXT,                             -- bounded tail (<=64 KiB) when log_ref='inline'
    log_digest  TEXT,                             -- sha256 hex of full captured log
    error_detail TEXT,                            -- spawn/classification detail, never raw env
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT verification_runs_attempt_unique UNIQUE (run_id, attempt, suite)
);
-- "Is the full suite set green at this SHA for this run?" — gate lookup.
CREATE INDEX IF NOT EXISTS idx_verification_runs_sha
    ON verification_runs (run_id, subject_commit_sha, suite);
-- Reconcile sweep: rows stuck 'running' whose worker/lease is gone.
CREATE INDEX IF NOT EXISTS idx_verification_runs_running
    ON verification_runs (status) WHERE status = 'running';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS verification_runs;
```
-- Reconciliation (2026-10-25): after each suite row completes, the service ALSO
-- inserts `evidence_artifacts(kind='test_results', producer='verifier')` in the
-- same tx (metadata.status 'all_green' only on full pass) — this is the row
-- VTX-10 gate condition C3 and the verifying→qa guard read. And whoever
-- updates `task_runs.commit_sha` (the VTX-7 agent-result recording path) emits
-- `run.evidence_invalidated.v1` in that same tx when a newer SHA supersedes
-- evidence bound to an older one — that event now has an owner.
-- +goose StatementEnd
```

Companion queries (`db/queries/verification.sql`): `InsertVerificationRun`, `FinishVerificationRun` (guarded `WHERE status='running'`), `ListVerificationRuns`, `GetSuiteSetAtSHA` (`SELECT suite,status … WHERE run_id=? AND commit_sha=?`), `MarkSuperseded` (`UPDATE … SET status='superseded' WHERE run_id=? AND commit_sha<>? AND status IN ('passed','failed','skipped')` — the ONLY mutation of terminal rows), `ListStaleRunning` (reconcile).

Status vocabulary: `passed`=exit 0, `failed`=nonzero exit (verdict on code), `error`=runner/infra failure (spawn, IO, lost lease), `timeout`=killed by deadline, `skipped`=suite short-circuited by an earlier mandatory failure, `superseded`=SHA drift marking.

## 4. Go package / API design

### 4.1 `internal/execcmd` — process boundary (mirrors `gitops` discipline)

Every subprocess execution for verification goes through this package; injectable `Runner` exactly like `gitops.Git.Runner`.

```go
// CmdSpec is a fully-resolved, allowlisted command. No shell. Ever.
type CmdSpec struct {
    Name string   // suite name, e.g. "make-test"
    Argv []string // e.g. {"make", "-C", dir, "test"} — dir already canonicalized
    Dir  string   // cwd = canonical worktree path
    Env  []string // minimal allowlist: PATH, HOME, GOPATH/GOCACHE, pnpm store vars
}

// Result of one suite run.
type Result struct {
    ExitCode  int
    TimedOut  bool
    Output    []byte  // captured combined stdout+stderr, bounded
    Truncated bool
    Duration  time.Duration
}

// Runner executes one spec under ctx deadline; the deadline maps to
// Result.TimedOut rather than an opaque ctx error.
type Runner interface {
    Run(ctx context.Context, spec CmdSpec) (Result, error)
}

// ProcessRunner is the production impl (os/exec); FakeRunner in tests.
// Capture is bounded: first 64 KiB kept for log_tail; full stream is tee'd to
// the artifact sink and hashed → log_ref + log_digest.
```

`SuitePlan` (same package) = the ordered allowlist: `make test-boundaries`, `make vet`, `make typecheck`, `make test`, `make build` — rendered as argv with `-C <worktree>`; a suite is marked `mandatory` (all five are, in MVP). Adding a suite = code change + migration-free row — the allowlist lives in code, not config, so nothing user-supplied reaches `exec` (NFR-SEC-001).

### 4.2 `internal/application` — `verification_ports.go` + `verification_service.go`

```go
type VerificationStore interface {
    BeginSuite(ctx context.Context, row VerificationRun) (VerificationRun, error)   // unique(run_id,attempt,suite) → idempotent replay returns existing
    FinishSuite(ctx context.Context, id string, f Finish) error                     // only status='running' → terminal
    SuiteSet(ctx context.Context, runID, sha string) ([]VerificationRun, error)
    MarkSuperseded(ctx context.Context, runID, currentSHA string) error
    StaleRunning(ctx context.Context, before time.Time) ([]VerificationRun, error)
}
// Reused: WorktreeStore (allocated path), WorktreeGit (HeadSHA), RegistryService, Clock.

type VerificationService struct {
    store    VerificationStore
    runner   execcmd.Runner
    git      WorktreeGit
    worktrees WorktreeStore
    registry *RegistryService
    layout   gitops.Layout
    sink     LogSink            // filesystem artifact sink; inline fallback
    cfg      VerificationConfig // per-suite timeout, stage timeout, log caps
    clock    Clock
    logger   *slog.Logger
}

// VerifyRun is THE entry point while run.state == 'verifying'. Idempotent on
// (run_id, attempt): a crashed stage re-enters with attempt+1.
func (s *VerificationService) VerifyRun(ctx context.Context, cmd VerifyCommand) (VerifyOutcome, error)

type VerifyCommand struct {
    RunID, TaskID string
    Attempt       int
    LeaseOwner    string
    LeaseEpoch    int64
}
type VerifyOutcome struct {
    AllPassed bool
    SHA       string
    Failed    []string  // suite names, for the retry prompt back to the agent
    Verdict   string    // passed | failed | error | timeout | stale_sha | lease_lost
    Retryable bool
    ResumeTo  string    // 'coding' or 'verifying' — see §6
}
```

`VerifyRun` flow:

1. Resolve worktree row (`status='allocated'`) → `CanonicalPath`; `HeadSHA(path)`.
2. **SHA gate:** if `HEAD != task_runs.commit_sha` → `MarkSuperseded` + verdict `stale_sha` → `schedule_retry` with `resume_state=coding` (the tree changed under us; agent/output must be re-baselined). Never auto-reverify a drifted tree — evidence must track the SHA the *agent* was asked to produce.
3. For each suite in order: `BeginSuite(status='running', commit_sha=sha)` → `runner.Run` (per-suite ctx timeout) → `FinishSuite(terminal status, exit_code, duration, log_ref, digest)`. Short-circuit: first mandatory `failed` marks remaining suites `skipped` (cheap; a re-run gets a fresh attempt).
4. Verdict → `TransitionRun`: all `passed` → `advance` verifying→qa (stays gated `operational:false` until VTX-9 — the transition *call* is issued by orchestration, and the flip happens in the QA lane); `failed` → `schedule_retry` `resume_state=coding` (+failed-suite list persisted on the run's error fields for the fix prompt); `error`/`timeout` → `schedule_retry` `resume_state=verifying`; lease rejection mid-stage → abort loop, mark current row `error` detail `lease_lost`, escalate process kill.

`LogSink` port (`internal/persistence` impl): writes captured output under `<EvidenceRoot>/<run_id>/<attempt>/<suite>.log`, returns `log_ref`; when sink write fails, fall back to `log_ref='inline'` + `log_tail` — evidence is never dropped for a storage fault.

## 5. State machine interactions (SRS §5)

```
 coding ──advance──▶ verifying ──┬─ all suites passed ──advance──▶ qa            [VTX-9 flips]
                                 ├─ suite failed  ──schedule_retry──▶ retry_wait ──retry_due──▶ resume_state=coding
                                 ├─ error/timeout ──schedule_retry──▶ retry_wait ──retry_due──▶ resume_state=verifying
                                 ├─ stale_sha     ──schedule_retry──▶ retry_wait ──retry_due──▶ resume_state=coding
                                 ├─ block(operator) ──▶ blocked ──resume──▶ verifying
                                 └─ cancel/fail ──▶ cancelled | failed
```

| Interaction | Action / actor | Notes |
|---|---|---|
| Enter stage | `advance` coding→verifying, `orchestrator` (issued by VTX-7) | Becomes `operational:true` **with this epic**, since `VerifyRun` is the real executor. |
| All suites pass at pinned SHA | `advance` verifying→qa, `orchestrator` | Requires full `passed` suite set at `commit_sha` (`GetSuiteSetAtSHA`); hop remains `operational:false` until VTX-9 lands a QA consumer — orchestration issues the call and the gate decides operability, this service only produces the verdict + evidence. |
| Suite `failed` (real test failure) | `schedule_retry`, `policy`; `resume_state=coding` | Agent must fix; failed-suite names + log digests feed the retry prompt. |
| `error`/`timeout`/`stale_sha` | `schedule_retry`, `policy`; `resume_state` per table | Infra flake → re-run verification; drift → back to coding. |
| Budget exhausted / non-retryable | `fail`, `policy` | Terminal; evidence rows remain for post-mortem + DLQ view. |
| Operator block/cancel mid-suite | `block`/`cancel` | In-flight suite process is killed (ctx cancel → `timeout`-class row `error`/`cancelled` detail); remaining rows `skipped`. |
| Crash mid-suite (worker dies) | reconciler | `StaleRunning` sweep marks `error` detail `worker_crash`; lease expiry lets a new attempt start — `UNIQUE(run_id,attempt,suite)` prevents split-brain double-writes for the same attempt. |

## 6. Error / retry semantics

| Outcome | Detection | Row status | Run action | Retryable |
|---|---|---|---|---|
| Suite exit 0 | `Result.ExitCode==0` | `passed` | continue / `advance` | n/a |
| Suite exit ≠ 0 | nonzero | `failed` | `schedule_retry` → `coding` | yes (agent fix) |
| Deadline hit | `Result.TimedOut` | `timeout` | `schedule_retry` → `verifying` | yes, bounded |
| Spawn failure | `runner.Run` err | `error` | `schedule_retry` → `verifying` | yes |
| HEAD drift pre-run | `HeadSHA != commit_sha` | `superseded` (+ marking rows) | `schedule_retry` → `coding` | yes |
| Lease lost / stale epoch | `TransitionRun` reject | `error` (`lease_lost`) | none — fenced out | n/a |
| Worker crash | `StaleRunning` sweep | `error` (`worker_crash`) | next attempt | yes |
| Log sink failure | sink err | terminal status kept, `log_ref='inline'` | none — degraded evidence, never blocks | n/a |

Fixed bounds (defaults, configurable): per-suite timeout 10 min; whole-stage timeout 30 min; `log_tail` ≤ 64 KiB; full log to sink (single-file cap 32 MiB, then truncate marker). No suite is ever re-run in place — a re-run is a new `attempt`, keeping every prior verdict auditable. Backoff/retry cadence is owned by VTX-11 (`schedule_retry` + `retry_due_at`); this service only declares `Retryable` + `ResumeTo`.

## 7. Test matrix (SRS §9 evidence)

§9 mapping: VTX-6..12 line ("Baseline tests … gate policy") and VTX-4/5-adjacent (worktree isolation already covered there). IDs become test names.

| ID | Test | Evidence for |
|---|---|---|
| EXEC-01 | `FakeRunner` exit 0 for all suites → 5 `passed` rows at pinned SHA, `GetSuiteSetAtSHA` green, outcome `passed` | happy path, §9 VTX-6..12 |
| EXEC-02 | `make test` exits 1 → row `failed`, later suites `skipped`, outcome `failed`, `resume=coding` | FR-ERR-001 classification |
| EXEC-03 | Runner timeout → row `timeout`, `resume=verifying` | bounded execution |
| EXEC-04 | Spawn error → `error`, retryable | infra class |
| EXEC-05 | HEAD drift pre-run → `stale_sha`, prior rows `superseded`, `resume=coding`; gate lookup at new SHA returns empty | FR-QA-003 + E2E-05 |
| EXEC-06 | Crash simulation: rows left `running`; `StaleRunning` marks `error`; new `attempt` inserts cleanly (UNIQUE honored) | NFR-REL-001, E2E-03 |
| EXEC-07 | Same `(run_id,attempt,suite)` replay → `BeginSuite` idempotent, no double row | exactly-once effect |
| EXEC-08 | `workspace_path` traversal/symlink → refused before exec | NFR-SEC-001 |
| EXEC-09 | Command allowlist: `SuitePlan` argv contains no caller/agent strings; injection-shaped input cannot reach `exec` (table test over spec fields) | NFR-SEC-001 |
| EXEC-10 | Log sink down → `log_ref='inline'` + `log_tail` ≤64 KiB + digest present | degraded evidence |
| EXEC-11 | Log capture bounded: flood output → `Truncated=true`, tail preserved, digest over full stream | research §4 backpressure analog |
| EXEC-12 | Lease rejection mid-stage → in-flight process killed, row `error`/`lease_lost`, no `advance` attempted | FR-SCH-002 |
| EXEC-13 | Real worktree integration (build-tagged): allocate via VTX-4 service, run `make -C <wt> vet`-class fast target, row persisted with real SHA | §9 "baseline tests" |
| EXEC-14 | `verifying` stage transitions: `coding→verifying` operational; `verifying→qa` still `ErrStageNotOperational` pre-VTX-9 | FR-REG-007 |
| EXEC-15 | Operator cancel mid-suite → process killed, current row terminal (`error`), rest `skipped`, run `cancelled` | E2E-07 (partial) |

## 8. Open decisions

| ID | Decision | Status |
|---|---|---|
| D8-1 | Fixed make-target suite allowlist in code (`test-boundaries, vet, typecheck, test, build`), no per-repo config | **DECIDED** — matches AGENTS.md verification protocol and this repo's single-module layout; a `suite` column keeps rows open to future entries without schema change. |
| D8-2 | Evidence keyed to exact SHA; drift ⇒ `superseded` marking + `resume=coding`, never silent re-verify | **DECIDED** — direct FR-QA-003 mechanism; keeps QA/gate reads a pure SHA lookup. |
| D8-3 | `failed`→resume `coding`, `error`/`timeout`→resume `verifying` | **DECIDED** — test verdict needs agent action; infra failure does not. |
| D8-4 | Log capture: full stream → filesystem sink + digest; ≤64 KiB inline tail fallback | **DECIDED** — bounded memory, tamper-evident digest, evidence survives sink outage. |
| D8-5 | One row per `(run_id, attempt, suite)`; verification `attempt` independent of agent `attempt` | **DECIDED** — verification can re-run on the same commit without a new coding attempt. |
| D8-6 | In-flight kill on lease loss/block; in-flight row → `error` not `running` | **DECIDED** — a fenced-out worker must not leave a green-looking row. |
| D8-7 | `verification_attempt` counter storage (column on `task_runs` vs computed `MAX(attempt)+1`) | **DECIDED** — computed `MAX(attempt)+1` via query; avoids new run-table writes/CAS contention. |
| D8-8 | Artifact/log storage root and retention (filesystem path vs object store) | **STAKEHOLDER** — binds to SRS OD-06 (RPO/retention). Filesystem sink under a configured `EvidenceRoot` is the recommended MVP; needs ops owner to confirm durability requirements. |
| D8-9 | Which suites are *mandatory* gate inputs vs advisory | **STAKEHOLDER** — gate policy is VTX-10's owner; recommendation: all five mandatory in MVP (matches `make` contract). |
| D8-10 | pnpm store / Go cache warm-up inside fresh worktrees (verification speed) | **STAKEHOLDER** — perf/cost call (NFR-PERF-001 TBD); recommend shared caches mounted read-write per-worker, per-run dirs. |
| D8-11 | Whether `verifying→qa` advance is issued by this service or the orchestrator on verdict receipt | **DECIDED** — orchestrator issues transitions after consuming `VerifyOutcome`; this service stays verdict-only, consistent with VTX-7's split. |
| D8-12 | Concurrent-suite execution (parallel targets) | **DECIDED** — sequential only in MVP: deterministic resource usage under the ≤3-run admission cap (FR-SCH-001); revisit if NFR-PERF-001 demands. |
