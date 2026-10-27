# VTX-12 — GitHub Draft PR Lifecycle — Design Specification

**Epic:** VTX-12 (Draft PR lifecycle)
**Status:** Design spec — implementation-grade, English
**SRS coverage:** §4.5 FR-PR-001 (+ supporting FR-QA-002/003, FR-ERR-001), §5 state model (`quality_gate → pr_draft → completed`), §6 data contracts, §7 GitHub API, §8 NFR-AUD-001 / NFR-REL-001 / NFR-SEC-001 / NFR-MAINT-001, §9 verification (VTX-6..12 band, E2E-01/02/04/05), §11 OD-05
**Prior art reused:** `infra/scripts/pr_pipeline.py` + `plans/reports/research-261023-0849-jira-pr-review-pipeline.md` (idempotency-by-external-truth, bounded retries, never auto-merge posture). The review-agent/Jira-handback half of that script is **out of scope** for VTX-12; it belongs to a later review epic.
**Grounding:** `apps/backend/internal/domain/registry.go`, `internal/application/registry_*`, `db/migrations/00001–00005`, `contracts/openapi/v1/openapi.yaml`, `contracts/events/v1/events.json`.

---

## 1. Overview

VTX-12 closes the autonomous pipeline: after the deterministic Quality Gate (VTX-10) records a **PASS verdict at an exact commit SHA**, the platform opens a **GitHub Draft Pull Request** against the configured base branch, records the PR identity durably, and transitions the run `quality_gate → pr_draft → completed`. Completion means "a draft PR exists and is recorded" — **never** merge and never deploy. A human reviews and merges on GitHub.

Hard invariants (all derived from SRS, restated here because they constrain every design choice):

1. **No PR without gate PASS.** The `advance` hop `quality_gate → pr_draft` is *PASS-gated* (SRS §5). The guard is a persisted `gate_decisions` row for `(run_id, commit_sha)`, not a flag in a request body.
2. **Exact-SHA binding (FR-QA-003).** Evidence evaluated SHA `X`; the PR must publish SHA `X`. If the remote head moved, the verdict is stale and the PR must not be created.
3. **Draft only, never merge.** There is no merge code path anywhere in VTX-12. `draft` is not an input — it is a constant `true`.
4. **At most one PR per run, no duplicate PRs** (E2E-02): dedupe on both the DB side (`UNIQUE(run_id)`) and the GitHub side (list-before-create + adoption).
5. **Every step audited and replay-safe** (NFR-AUD-001/NFR-REL-001): PR creation writes its durable record and an outbox event in the same transaction that any run-state bookkeeping happens in; GitHub is never called inside a DB transaction.

### 1.1 Sequence

```
```text
quality_gate (gate wrote verdict=pass for run+sha)
   │  [1] OpenDraftPR begins (orchestrator, lease held)
   │  [2] verify verdict (run_id, sha, verdict='pass')           → else abort
   │  [3] verify remote: repo exists, base ref exists,           → else abort
   │      remote head(branch) == run.commit_sha                  → else abort
   │  [4] advance run quality_gate → pr_draft (lease-gated,
   │      metadata: gate_decision_id, expected sha)
   │  [5] dedupe: pull_requests row? adopt open PR for head?
   │  [6] POST /repos/{o}/{r}/pulls {draft:true}                 → GitHub call
   │  [7] tx: insert pull_requests + task_runs.pr_url +
   │      outbox pull_request.opened.v1
   │  [8] complete run pr_draft → completed (completion policy:
   │      pull_requests.status='open' recorded)
completed
```

(the `text` fence above is a plain ASCII sequence diagram)

Steps [4], [7], [8] are three separate transactions on purpose: the GitHub call ([6]) sits between durable states so a crash leaves either "still in quality_gate" (safe replay) or "PR exists, run in pr_draft" (reconciled by [5] adoption on retry).

---

## 2. SRS Coverage Table

| SRS ID | Requirement (paraphrased) | Design decision |
|---|---|---|
| FR-PR-001 | Draft PR only after gate PASS; no auto merge/deploy | §4.3 `PRService.OpenDraftPR` requires a persisted `gate_decisions` PASS row for the run's exact SHA before the `advance` transition is attempted; `GitHubPort` exposes no merge mutation; `draft` is a hardcoded `true`, not a parameter (§4.1, §6.4). |
| FR-QA-002 | Gate PASS only from required conditions, not an LLM comment | VTX-12 does not trust a caller's claim: it reads `gate_decisions` (written only by the gate implementation identity) and refuses if absent/mismatched (§3 `gate_decisions`, §5.2 guard). |
| FR-QA-003 | SHA changed after QA ⇒ evidence invalid | Two checks: (a) `gate_decisions.commit_sha == task_runs.commit_sha` at transition time; (b) remote `heads/{branch}` SHA == `commit_sha` at PR-create time. Either mismatch ⇒ `gate_decision_stale` / `head_sha_mismatch`, no PR, run parked `blocked` (§6.3). |
| FR-ERR-001 | Bounded retry, terminal failure with evidence | Error taxonomy §6.2 splits retryable GitHub faults (rate limit, 5xx, network) from permanent ones (evidence/config). Retry budget `PR_MAX_ATTEMPTS=5`, exp backoff honoring `Retry-After`; exhaustion ⇒ `fail` transition with `error_code` + `pull_requests.status='failed'` + audit (§6.5). |
| FR-REG-005 | TransitionRun: actor auth, version CAS, lease fencing, atomic state+audit+outbox | VTX-12 mutates run state **only** through the existing `POST /registry/runs/{id}/transitions` / `RegistryTransitioner` surface as actor `orchestrator` with `expected_version` + `expected_lease_epoch`. No direct `task_runs` writes except the additive `pr_url` column write inside the same tx as `pull_requests` insert (§3, §5). |
| FR-REG-006 / §5 | States `quality_gate`, `pr_draft`, `completed`; blocked/retry_wait resumable | VTX-12 flips `operational=true` on the two existing contract rows `quality_gate --advance--> pr_draft` and `pr_draft --complete--> completed` (currently `ErrStageNotOperational`), and adds the evidence precondition outside the state table (§5.2). No new states, no new actions. |
| FR-REG-007 | Unimplemented downstream stages must not fake success | The `operational` flag flip only happens together with the real PR implementation; the `stage_not_operational` path remains for any caller that reaches the hop without VTX-12 evidence (§5.2, OD-05). |
| §7 GitHub API | Repo/ref/commit checks, draft PR, CI run/required status | `GitHubPort` §4.2: `GetRepo`, `GetRefSHA` (base + head), `GetCommit`, `ListOpenPRsForHead`, `CreateDraftPR`, `GetPR`, `GetCheckRollup`. CI rollup persisted via `pull_requests.ci_status` through `SyncPullRequest` (§4.4). |
| §6 outbox | Versioned payloads | New `pull_request.opened.v1` outbox event written in the same tx as the `pull_requests` row (§3, §4.3). |
| NFR-AUD-001 | issue→intent→run→SHA→PR traceability; no tokens in logs | `pull_requests.run_id` + `task_runs.pr_url` give one-hop joins; audit metadata on the `advance`/`complete` transitions carries `gate_decision_id`, `pr_url`, `pr_number`. GitHub token lives in env only, never logged/serialized (§6.6). |
| NFR-REL-001 | No lost intent; replay safe; fault injection | Crash-recovery §6.5: pre-GitHub crash replays cleanly; post-GitHub/pre-DB crash is healed by `ListOpenPRsForHead` adoption; `UNIQUE(run_id)` caps records at one. |
| NFR-SEC-001 | TLS, secret redaction, least privilege | GitHub client is HTTPS-only (configurable base for GHES, `https://` enforced); PAT/fine-grained token scoped `pull_requests:write, contents:read, checks:read` on the single repo (§4.2, §6.6). |
| NFR-MAINT-001 | Deterministic migrations, sqlc, contract conformance | Migration 00012 is additive-only Goose SQL; new queries live in `db/queries/pullrequests.sql`; OpenAPI delta in `docs/design/contract-drafts/vtx12-13-openapi.yaml` (§7). |
| UC-05 | Gate PASS → evidence → Draft PR; human review/merge separate | §1.1 sequence is literally UC-05; the spec ends at `completed` — review/merge is GitHub-side human work. |
| E2E-01 | …QA PASS → Draft PR | PR-14 integration test (§7). |
| E2E-02 | no duplicate PR on replay | PR-05/PR-06 (§7): `UNIQUE(run_id)` + adoption path. |
| E2E-04 | QA failure → no Draft PR | PR-03 (§7): no verdict or `verdict='fail'` ⇒ precondition error, zero GitHub calls (asserted via fake transport counter). |
| E2E-05 | SHA drift invalidates evidence | PR-04 (§7): remote head ≠ `commit_sha` ⇒ `head_sha_mismatch`, no PR, run `blocked`. |
| OD-05 | Gate semantics for downstream stages | Resolved for this stage: a hop is operational only when its producer writes verified evidence first — `stage_not_operational` stays the default elsewhere (§8 D3). |

---

## 3. Data Model / DDL Deltas (future migration `00012_draft_pr.sql`)

Additive only (FR-REG-009): no drops, no renames. Two new tables + one additive column + one additive outbox event type. `gate_decisions` is the **evidence contract VTX-12 reads**; the writer is the VTX-10 gate implementation — if the VTX-10 lane lands a differently-named table, VTX-12's `GateEvidenceReader` port adapts to it, the read contract (run, sha, verdict, policy) does not change.

```sql
-- +goose Up
-- VTX-12 does NOT define its own verdict store. It reads `gate_decisions`
-- (append-only, VTX-10 section 3): the effective PASS is the latest
-- evaluated_at row whose decision='pass' for (run_id, subject_commit_sha).
-- Reconciled 2026-10-25: supersedes the earlier local `gate_decisions` draft.

CREATE TABLE IF NOT EXISTS pull_requests (
    pr_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- One PR record per run, forever: doubles as the create-idempotency key.
    run_id UUID NOT NULL UNIQUE REFERENCES task_runs(run_id) ON DELETE CASCADE,
    repo_owner TEXT NOT NULL,
    repo_name TEXT NOT NULL,
    pr_number INT,                          -- NULL only while status='requested'
    pr_url TEXT,
    draft BOOLEAN NOT NULL DEFAULT TRUE CHECK (draft IS TRUE),  -- structurally impossible to persist non-draft
    head_branch TEXT NOT NULL,
    head_sha TEXT NOT NULL,                 -- SHA the PR was opened at; == task_runs.commit_sha at create
    base_branch TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN
        ('requested', 'open', 'closed', 'merged', 'failed')),
    github_pr_id BIGINT,                    -- GitHub's immutable pull id, when known
    ci_status TEXT CHECK (ci_status IS NULL OR ci_status IN
        ('none', 'pending', 'success', 'failure', 'error')),
    last_synced_at TIMESTAMPTZ,
    opened_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    error_code TEXT,                        -- taxonomy in §6.2
    error_detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT pull_requests_github_unique UNIQUE (repo_owner, repo_name, pr_number)
);
CREATE INDEX IF NOT EXISTS idx_pull_requests_status
    ON pull_requests (status) WHERE status IN ('requested', 'open');

-- Denormalized one-hop link so the run row itself satisfies "PR link recorded
-- on run" even if pull_requests is archived later. Written in the same tx as
-- the pull_requests insert.
ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS pr_url TEXT;

-- Track redrive visibility for ops (see vtx-13-dashboard.md §3).
-- (shared delta: defined here once; if 00012 lands before the dashboard
-- migration, the dashboard spec's ALTER is skipped as IF NOT EXISTS)
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS redrive_count INT NOT NULL DEFAULT 0;
```

**New outbox event type** `pull_request.opened.v1` — payload:

```json
{
  "event_id": "<outbox_id>", "run_id": "uuid", "task_id": "uuid",
  "intent_id": "uuid", "pr_number": 123, "pr_url": "https://github.com/o/r/pull/123",
  "repo": "o/r", "head_sha": "abc…", "base_branch": "dev",
  "draft": true, "correlation_id": "…", "occurred_at": "RFC3339"
}
```

`contracts/events/v1/events.json` gets a `PullRequestOpenedV1` definition + `pull_request.opened.v1` added to the `PlatformEvent.type` enum (coordinator merge note is in the contract draft file).

New queries file `db/queries/pullrequests.sql` (sqlc): `InsertPullRequest`, `GetPullRequestByRun`, `GetPullRequestByNumber`, `MarkPullRequestStatus`, `UpdatePullRequestSync`, `SetRunPrURL`, `GetGateVerdict(run_id, sha)`, `ListPullRequestsByStatus`.

---

## 4. Go Package / API Design

### 4.1 Layout (all under `apps/backend/internal/`)

```text
internal/integrations/github/
    client.go          // GitHubPort production impl — REST v3, net/http, no third-party SDK
    client_test.go     // httptest fakes: 200/201/403/422/429/5xx matrix
internal/application/
    pr_ports.go        // GitHubPort, PRStore, GateEvidenceReader + sentinels
    pr_service.go      // PRService: OpenDraftPR, SyncPullRequest, ReconcileRequested
    pr_service_test.go
internal/persistence/
    pullrequests.go    // pgx impl of PRStore + GateEvidenceReader
internal/transport/http/
    handlers_pr.go     // read endpoints + sync endpoint (§4.5)
internal/config/
    github.go          // ValidateGitHub() — fail-closed like ValidateRegistry
```

`internal/github` is deliberately placed under `internal/integrations/github/` to mirror `internal/integrations/jira/` (one external system per package, client only — zero business rules).

### 4.2 `GitHubPort` (domain-facing interface, in `pr_ports.go`)

```go
type GitHubPort interface {
    // Repo/ref/commit checks (SRS §7). All return ErrGitHubNotFound for 404.
    GetRepo(ctx, owner, repo string) (RepoRef, error)
    GetRefSHA(ctx, owner, repo, branch string) (string, error)      // heads/{branch}
    GetCommit(ctx, owner, repo, sha string) (CommitInfo, error)
    // Dedup + adoption (E2E-02).
    ListOpenPRsForHead(ctx, owner, repo, headBranch string) ([]PRRef, error)
    // The only write in the entire port. draft is not a parameter.
    CreateDraftPR(ctx context.Context, owner, repo string, in CreatePRInput) (PRRef, error)
    // CI rollup (SRS §7 "CI run/required status").
    GetPR(ctx, owner, repo string, number int) (PRRef, error)
    GetCheckRollup(ctx, owner, repo, sha string) (CheckRollup, error)
}
```

- Implementation: `net/http` + `encoding/json`; endpoints `/repos/{o}/{r}`, `/git/ref/heads/{b}`, `/commits/{sha}`, `/pulls`, `/pulls?head=o:branch&state=open`, `/commits/{sha}/check-runs` + `/commits/{sha}/status` (check-runs preferred, combined-status fallback — see §8 D6).
- Auth: `Authorization: Bearer $GITHUB_TOKEN`; `Accept: application/vnd.github+json`; `X-GitHub-Api-Version: 2022-11-28`.
- The client maps `Retry-After`, `403 + rate-limit headers`, `5xx`, `422` onto the typed errors of §6.2. Timeout: 15s per call, caller cancels via ctx.
- **There is no `MergePR`, `UpdateBranch`, or `draft` input.** `CreateDraftPR` serializes `"draft": true` unconditionally.

### 4.3 `PRService` (application layer)

```go
func (s *PRService) OpenDraftPR(ctx, cmd OpenPRCommand) (PRResult, error)
// cmd: RunID, LeaseOwner, LeaseEpoch, ExpectedVersion (for the advance hop),
//      Actor=orchestrator, CorrelationID, PRTitle, PRBody

func (s *PRService) SyncPullRequest(ctx, runID string) (PRView, error)
// refresh pr status + ci_status from GitHub; called by the sync endpoint
// and by the post-create snapshot.

func (s *PRService) ReconcileRequested(ctx) error
// sweeper: pull_requests.status='requested' rows older than N — retry or
// adopt; covers the crash window between GitHub success and DB commit.
```

Steps (mirroring §1.1):

1. Load run (reader). Precondition A: `state == quality_gate`. Precondition B: `GetGateVerdict(run_id, run.commit_sha)` exists, `verdict='pass'`. Else `ErrPRPrecondition{code}` — **no GitHub call made**.
2. Resolve `owner/repo` from `registry_tasks.repo_url` (parse `https://github.com/{o}/{r}[.git]`); enforce `GITHUB_REPO_ALLOWLIST` when configured; derive `base_branch` from config (`PR_BASE_BRANCH`, default `dev` — same default as `pr_pipeline.py`).
3. Remote checks: `GetRepo` → `GetRefSHA(base)` → `GetRefSHA(head_branch)` must equal `run.commit_sha`. Mismatch ⇒ `ErrHeadSHAMismatch`.
4. `TransitionRun(action=advance, expected_version, expected_lease_epoch, metadata={gate_decision_id, commit_sha})` → run enters `pr_draft`.
5. Dedupe: `GetPullRequestByRun` → return existing (`idempotent`); else `ListOpenPRsForHead` → adopt if found (insert `pull_requests` with status from GitHub, `draft` verified `true`; a found non-draft PR is adopted but logged as anomaly — the platform still did not merge it).
6. `CreateDraftPR` (title: `[{issue_key}] {summary}`, body: evidence footer — gate verdict id, SHA, run id, `<!-- vtx-pipeline -->` marker).
7. One tx: `InsertPullRequest` + `SetRunPrURL` + outbox `pull_request.opened.v1`. **No GitHub call inside the tx.**
8. `TransitionRun(action=complete)` with completion policy: `pull_requests.status='open'` — service verifies the persisted row, not memory.

### 4.4 `SyncPullRequest` (CI status, SRS §7)

Reads `GetPR` + `GetCheckRollup(head_sha)`; writes `ci_status`, `status` (`open`/`closed`/`merged` — observed, never caused), `last_synced_at`. Invoked: (a) once right after creation, (b) via `POST /api/v1/pull_requests/{run_id}/sync` (operator/policy actor), (c) optionally by a periodic sweeper — flagged STAKEHOLDER §8 D7 since GitHub webhooks would need ingress the MVP network may not expose.

### 4.5 HTTP surface (registry-token group, like worktree routes)

| Route | Actors | Purpose |
|---|---|---|
| `GET /api/v1/registry/runs/{run_id}/pull_request` | all 4 (read) | PR record for a run; 404 when none |
| `GET /api/v1/pull_requests?status=&limit=&cursor=` | all 4 (read) | Ops list for dashboard |
| `POST /api/v1/pull_requests/{run_id}/sync` | orchestrator, policy | Pull fresh status/CI; rate-limited (60s per run, 409 `sync_throttled`) |

Schemas in the contract draft (`PullRequest`, `CheckRollup`). Mutations of run state stay on the existing `transitions` endpoint — VTX-12 adds **no** new state-mutation route.

### 4.6 Config (fail-closed, `ValidateGitHub()` in `internal/config/github.go`)

`GITHUB_TOKEN` (required when VTX-12 wired), `GITHUB_API_BASE_URL` (default `https://api.github.com`, must be `https://`), `GITHUB_REPO_ALLOWLIST` (CSV `owner/repo`, empty = any github.com repo — dev default), `PR_BASE_BRANCH` (default `dev`), `PR_MAX_ATTEMPTS` (default `5`), `PR_RETRY_BASE_DELAY` (default `30s`). Token must differ from all registry actor tokens and the dashboard/gateway tokens (same hygiene rule as `ValidateRegistry`).

---

## 5. State Machine Interactions (SRS §5)

### 5.1 Contract rows VTX-12 operationalizes

| From | Action | To | Actor | Guard added by VTX-12 |
|---|---|---|---|---|
| `quality_gate` | `advance` | `pr_draft` | `orchestrator` | Persisted `gate_decisions` PASS at `run.commit_sha` (checked by `PRService` before calling `TransitionRun`; `gate_decision_id` recorded in transition metadata so the audit row is self-describing) |
| `pr_draft` | `complete` | `completed` | `orchestrator` | Completion policy: `pull_requests.status='open'` row exists for the run |

Both flips (`operational: false → true` in `domain/registry.go transitionTable`) happen **in the VTX-12 implementation commit**, together with the service that enforces the guards — never earlier (OD-05). The state machine itself gains no rows, states, or actions; the evidence precondition is enforced by the calling service so `TransitionRun` stays a pure CAS+lease+auth kernel and remains directly testable.

### 5.2 Failure paths into the machine

| Situation | Transition used | Result state |
|---|---|---|
| Transient GitHub fault, attempts left | `schedule_retry` (policy actor) | `retry_wait` (resume_state=`quality_gate`) |
| Retry due | `retry_due` (scheduler/policy) | back to `quality_gate` |
| Permanent evidence fault (`head_sha_mismatch`, `gate_decision_stale`) | `block` (policy actor) with reason + `error_code` | `blocked` — resumable by operator after re-verification |
| Retry budget exhausted | `fail` (policy actor) | `failed` (terminal), `error_code=pr_retry_exhausted` |
| Operator stops the run | existing `cancel`/`resume` | unchanged semantics |

`block` (not `fail`) for SHA drift is deliberate: a new commit is a legitimate event — the run is parked until re-verification produces a fresh verdict, which is exactly what `blocked` + `resume` exists for.

### 5.3 Ordering with the lease

All transitions require the orchestrator's unexpired lease + expected epoch (`requiresLease` already covers `advance`/`complete`). GitHub calls happen **while holding the lease**; a lost lease between [6] and [8] is healed by `ReconcileRequested`/`ListOpenPRsForHead` adoption — the lease gates DB writes, never GitHub reads.

---

## 6. Error / Retry Semantics

### 6.1 Error classes → typed Go errors (`pr_ports.go` sentinels)

`ErrPRPrecondition` (evidence), `ErrRepoNotFound`, `ErrRepoForbidden`, `ErrBaseRefMissing`, `ErrHeadRefMissing`, `ErrHeadSHAMismatch`, `ErrPRAlreadyExists` (internal adoption signal), `ErrGitHubRateLimited`, `ErrGitHubUnavailable`, `ErrGitHubValidation` (422 other), `ErrPRNotFound`.

### 6.2 `error_code` taxonomy (persisted on `pull_requests.error_code` / `task_runs.error_code`)

| code | retryable | cause |
|---|---|---|
| `gate_evidence_missing` | no | no verdict row for (run, sha) |
| `gate_decision_stale` | no | verdict sha ≠ run sha |
| `head_sha_mismatch` | no | remote head moved post-gate |
| `repo_not_found` / `repo_forbidden` / `repo_not_allowed` | no | repo/permissions/allowlist |
| `base_ref_missing` / `head_ref_missing` | no | base branch gone / agent never pushed |
| `github_rate_limited` | yes | 403/429 + rate-limit headers; honor `Retry-After` |
| `github_unavailable` | yes | 5xx / network / timeout |
| `github_validation_failed` | no | 422 not attributable to existing-PR |
| `pr_retry_exhausted` | — | terminal wrapper after budget spent |

### 6.3 Decision rules

- **Retryable** ⇒ `schedule_retry`, `retry_due_at = now + min(30s·2^attempt, 15m)` or `Retry-After` when larger; `attempt++` (existing `RunUpdate` machinery already increments on `schedule_retry`).
- **Non-retryable evidence faults** ⇒ `block` with reason carrying `error_code` (operator sees it in the dashboard error disposition, FR-UI-002).
- **Exhaustion** (`attempt ≥ PR_MAX_ATTEMPTS`) ⇒ `fail` + `error_code=pr_retry_exhausted`; `pull_requests.status='failed'` if a row exists.
- **422 "A pull request already exists"** ⇒ never retried blindly: `ListOpenPRsForHead` → adopt; adoption found nothing ⇒ `github_validation_failed`.

### 6.4 Never-merge enforcement (structural)

- `GitHubPort` has no merge/close-write methods.
- `pull_requests.draft` has `CHECK (draft IS TRUE)` — a non-draft row cannot be persisted.
- `SyncPullRequest` may *observe* `merged`/`closed` (a human did it on GitHub) but contains no call that could cause it.

### 6.5 Crash recovery / replay (NFR-REL-001)

| Crash point | State on restart | Recovery |
|---|---|---|
| before [4] advance | run in `quality_gate`, no PR row | full replay; verdict check re-runs |
| [4]–[6] | run in `pr_draft`, no PR row | `OpenDraftPR` re-enters at dedupe step; creates PR |
| [6]–[7] | PR exists on GitHub, no row | `ReconcileRequested`/retry → `ListOpenPRsForHead` finds it → adopt row → complete |
| after [7] | row exists | `OpenDraftPR` returns existing (idempotent) |
| lease lost mid-flight | transitions reject `stale_lease` | new owner re-runs; adoption heals any GitHub side effect |

### 6.6 Security notes

Token only from env; client never logs `Authorization`; `error_detail` stores GitHub's sanitized `message` field only (GitHub error payloads do not echo secrets, but the field is length-bounded at 500 chars and logged, never the raw body). Allowlist check happens before any token-carrying request leaves.

---

## 7. Test Matrix (mapped to SRS §9)

Evidence band: **VTX-6..12** ("Baseline tests, real agent integration, independent QA SHA evidence, gate policy, Draft PR") + E2E-01/02/04/05. Naming follows repo convention (`*_test.go` alongside code, integration tests hit real Postgres).

| ID | Test | Layer | SRS evidence |
|---|---|---|---|
| PR-01 | No verdict row ⇒ `gate_evidence_missing`, zero GitHub calls (fake transport counter) | unit svc | E2E-04, FR-PR-001 |
| PR-02 | `verdict='fail'` ⇒ same abort | unit svc | E2E-04 |
| PR-03 | Verdict sha ≠ run sha ⇒ `gate_decision_stale`, run `blocked` | unit+domain | E2E-05, FR-QA-003 |
| PR-04 | Fake remote head ≠ commit_sha ⇒ `head_sha_mismatch`, no create call | unit svc | E2E-05 |
| PR-05 | `UNIQUE(run_id)` second insert rejected; `OpenDraftPR` returns existing row | pgx integ | E2E-02 |
| PR-06 | Pre-existing open PR for head ⇒ adopted, `CreateDraftPR` never called | unit svc | E2E-02 |
| PR-07 | 403+rate-limit ⇒ `github_rate_limited` ⇒ `schedule_retry` at `Retry-After` | unit svc | FR-ERR-001 |
| PR-08 | 5xx ×5 ⇒ exhausted ⇒ `fail` + `pr_retry_exhausted` + evidence row | unit svc | FR-ERR-001 |
| PR-09 | 422 existing-PR ⇒ adoption path ⇒ success not error | unit svc | E2E-02 |
| PR-10 | Crash-simulation: inject `pull_requests`-insert failure after fake GitHub 201 ⇒ rerun ⇒ adoption heals | integ | NFR-REL-001 |
| PR-11 | `advance` with wrong lease epoch ⇒ `stale_lease`; wrong actor ⇒ `forbidden` | existing registry suite + new case | FR-REG-005 |
| PR-12 | Transition metadata records `gate_decision_id`; outbox `pull_request.opened.v1` payload parses against `events.json` schema | integ | NFR-AUD-001, §6 |
| PR-13 | `pull_requests.draft` CHECK rejects `false` insert (direct SQL) | pgx integ | FR-PR-001 |
| PR-14 | Happy path: verdict+matching sha ⇒ `advance` ⇒ draft PR ⇒ row + `pr_url` + event ⇒ `complete` ⇒ `completed` | integ (httptest GitHub) | E2E-01 |
| PR-15 | `SyncPullRequest` maps check-runs rollup to `ci_status`; human-closed PR ⇒ `status='closed'` observed | unit svc | SRS §7 |
| PR-16 | API: GET pull_request 404/200, sync endpoint actor-forbidden for scheduler token | HTTP integ | SRS §7 typed errors |
| PR-17 | Migration 00012 up/down on populated DB (00001–00005 applied + seeded runs) | migration test | FR-REG-009, NFR-REL-001 |
| PR-18 | Contract lint: openapi delta schemas validate; `pull_request.opened.v1` in `events.json` | CI contract check | NFR-MAINT-001 |

---

## 8. Open Decisions

| # | Decision | Status | Rationale / owner |
|---|---|---|---|
| D1 | Evidence contract: `gate_decisions(run_id, commit_sha, verdict, policy_version, evidence JSONB)` — VTX-10 writes, VTX-12 reads | **DECIDED** | Minimal durable binding for the PASS-gated hop; if VTX-10 ships a different store, only `GateEvidenceReader` adapts. Coordination note for the VTX-10 lane, not a blocker. |
| D2 | Two transitions (advance→pr_draft, complete→completed) with the GitHub call *between* them | **DECIDED** | Keeps the external call out of every DB tx; each crash point lands on a resumable or adoptable boundary (§6.5). |
| D3 | Precondition enforced in `PRService`, not inside `domain.Evaluate` | **DECIDED** | The state kernel stays pure (CAS/lease/auth). Evidence checks need reads the domain layer must not do. OD-05 resolved for this stage only. |
| D4 | SHA drift ⇒ `block`, not `fail` | **DECIDED** | Drift means "new work needs re-gating", which is resumable by definition; `fail` would falsely signal terminal exhaustion (FR-ERR-001). |
| D5 | Adoption over creation on 422/list-hit | **DECIDED** | Reused from `pr_pipeline.py` research: external truth (open PR for head) beats process state. Guarantees E2E-02 across crashes and double-drivers. |
| D6 | CI rollup via check-runs API, combined-status fallback | **DECIDED** | GitHub Actions posts check runs; legacy statuses still exist on some repos — read-only union keeps the dashboard honest either way. |
| D7 | CI sync mechanism: manual/scheduled `sync` endpoint only; **no inbound GitHub webhook in MVP** | **STAKEHOLDER** | Needs ingress config + webhook secret mgmt on a possibly private deployment (NFR-PRIV-001). Owner: Platform Administrator + Tech Lead. Fallback documented §4.4. |
| D8 | GitHub auth material: fine-grained PAT vs GitHub App installation token | **STAKEHOLDER** | PAT is the documented MVP path (env `GITHUB_TOKEN`); App tokens give least-privilege + expiry but need app registration. Owner: Platform Administrator (ties to NFR-SEC-001, OD-06). |
| D9 | PR body template / `<!-- vtx-pipeline -->` marker contents | **DECIDED** | Marker reused from `pr_pipeline.py` convention for future review-agent discovery; body = run id, issue key, exact SHA, gate verdict id — no secrets, no Jira-internal URLs. |
| D10 | Who calls `OpenDraftPR`: Temporal activity vs dispatcher-side orchestrator service | **STAKEHOLDER** | Depends on OD-04 (Temporal availability for the pipeline phase). The service is orchestrator-agnostic; the caller must simply hold the run lease + use actor `orchestrator`. Owner: orchestration lane (VTX-5/VTX-11). |
