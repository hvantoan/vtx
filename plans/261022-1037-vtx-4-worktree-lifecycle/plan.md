# VTX-4 — Git Worktree Lifecycle & Lease

Status: in_progress
Issue: VTX-4 (AUTO-03)
Branch: `vtx-4/worktree-lifecycle` (target: `dev`)
Worktree: `/home/vif/workspace/vtx-vtx4`

## Locked scope (advise-confirmed reframing)

Decisions locked in `/ak:advise` interview 2026-10-22:

| # | Decision | Choice |
|---|----------|--------|
| 1 | Lease ownership | **(b)** VTX-4 acquires/renews `task_runs.lease_*` itself inside the allocation tx — fills the VTX-3 gap (columns exist, no writer today) |
| 2 | API surface | **(c)** Go package + Temporal activities + HTTP handlers (`handlers_registry.go` pattern) |
| 3 | Dirty cleanup | **(a)** Any dirt (tracked or untracked) → quarantine; only clean worktrees get `git worktree remove` |
| 4 | Reconciliation | **(c)** Boot sweep at worker start + Temporal-scheduled periodic reconcile |
| 5 | Repo scope | **(b)** Per-run repo — VTX-4 manages clone/fetch of the parent repo (cache), not a fixed configured repo |
| 6 | Repo identity | **(b)** Add `registry_tasks.repo_url` (additive migration); `UpsertRegistryTask` gains the param |

## VTX-3 ↔ VTX-4 compatibility matrix

| Concept | VTX-3 owns | VTX-4 uses / adds |
|---|---|---|
| Run identity | `task_runs.run_id`, `task_id`, `intent_id` | `worktrees.run_id` UNIQUE FK — one worktree row per run, same-run idempotent |
| Active-per-task | `idx_task_runs_active_per_task` | new `idx_worktrees_active_per_task` partial unique on `status='allocated'` |
| Lease | `task_runs.lease_owner/lease_epoch/lease_expires_at` (no writer) | new sqlc queries `AcquireRunLease`, `RenewRunLease`, `ReleaseRunLease` write them |
| Branch | `task_runs.branch` | `SetRunBranch` writes `vtx/<issue_key>/<run8>` |
| Task identity | `registry_tasks` (site, issue, key, project) | `+ repo_url` column; `UpsertRegistryTask` extended |
| Repo path | — (legacy `tasks.workspace_path` NOT reused) | `worktrees.path` |
| Tx boundary | `queries.WithTx`, service-owned | same: lease + worktree row + branch in one tx |

## Requirements

1. Migration `00005_worktrees.sql` — additive only, restart-safe (`IF NOT EXISTS`), real `Down`.
2. `worktrees` table: `worktree_id`, `run_id UNIQUE`, `task_id`, `status` (`allocated|released|quarantined`), `path`, `branch`, `repo_url`, `base_sha`, `quarantine_reason`, timestamps.
3. `WorktreeService.Allocate(ctx, runID, owner)`: single tx — `FOR UPDATE` run lock → reject terminal → acquire lease (epoch+1, expires now+TTL) → git ops (ensure repo cache, `git worktree add`) → insert row → `SetRunBranch`. Idempotent: existing `worktrees.run_id` row returned as-is.
4. Repo cache: `<repo_cache_root>/<url-key>` clone + fetch before add.
5. Deterministic path `<worktree_root>/<run_id>` — makes crash-orphan dirs identifiable by reconcile.
6. `Cleanup(ctx, runID)`: run terminal/lease lost → dirty ⇒ quarantine (`mv` to quarantine root, status `quarantined`), clean ⇒ `git worktree remove`, status `released`.
7. `Reconcile(ctx)`: DB scan (allocated rows whose run is terminal or lease expired) + disk sweep (dirs under root with no matching row) → cleanup/quarantine.
8. Surfaces: `internal/application/worktree_service.go`, `internal/persistence/worktree.go`, `internal/gitops/` (os/exec git), `orchestration` activities `AllocateWorktree`/`CleanupWorktree`/`ReconcileWorktrees` + `WorktreeReconcileWorkflow`, worker: boot sweep + `ScheduleClient` interval reconcile, `handlers_worktree.go` behind `RegistryActorMiddleware`.
9. Config: `WORKTREE_ROOT`, `WORKTREE_QUARANTINE_ROOT`, `REPO_CACHE_ROOT`, `WORKTREE_LEASE_TTL`, `WORKTREE_RECONCILE_INTERVAL`, `WORKTREE_ACTOR_TOKENS` (reuse registry tokens if absent).
10. Tests: concurrent allocate (same run + distinct runs same task), duplicate retry idempotent, stale-epoch rejection, crash window (orphan dir sweep), clean/dirty cleanup, symlink/path traversal safety, migration up/down/up on populated VTX-3 schema.

## Non-goals

Scheduling decisions (VTX-5), agent execution (VTX-7), production migrations, force-delete of dirty worktrees, historical migration rewrites, reuse of legacy `tasks` table.

## Done means

- `make test-boundaries`, `make vet`, `make test-backend` green; `make test-integration` green with a throwaway `TEST_DATABASE_URL` (or BLOCKED with evidence if no Postgres available).
- Migration applies clean over populated VTX-3 schema; down/up restart-safe.
- Exactly one `allocated` worktree per task enforced by DB; duplicate allocate returns the same row.
- Dirty worktree is never force-removed.

## Phases

1. Migration + queries + `sqlc generate`
2. `internal/gitops` (real git ops, path safety)
3. Store (`persistence/worktree.go`) + service + ports
4. Orchestration activities + worker wiring (boot sweep + schedule)
5. HTTP handlers + routes
6. Unit tests + integration tests
7. Checks, docs (`docs/runbooks/`, contract notes), commit, push, PR, review, merge per `--ship --dev`
