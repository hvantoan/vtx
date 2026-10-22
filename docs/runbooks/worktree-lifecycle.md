# VTX-4 — Git Worktree Lifecycle & Lease

Contract decisions, operational surface, and VTX-5/VTX-7 handoff for the
worktree lifecycle service (migration `00005_worktrees.sql`).

## Ownership boundaries

| Layer | Owns |
|---|---|
| VTX-3 | `task_runs` (run authority), `task_runs.lease_*` columns, `task_runs.branch`, state machine |
| VTX-4 | `worktrees` table, writing `task_runs.lease_*` (first writer — VTX-3 defined the columns but never wrote them), worktree dir lifecycle, repo cache |
| VTX-5 | Scheduling/admission; expected caller of the allocate/release surface |
| VTX-7 | Executes inside the allocated path/branch |

## Contract decisions (locked 2026-10-22)

1. **VTX-4 acquires the run lease.** `AllocateWorktree` bumps
   `task_runs.lease_epoch`, sets `lease_owner` and `lease_expires_at` inside
   the same transaction as the worktree row. The epoch is the fencing token:
   any holder of an older epoch is stale.
2. **Repo identity lives on `registry_tasks.repo_url`** (additive column).
   The VTX-2 intent payload does not carry a repo; the first allocation binds
   it (`SetRegistryTaskRepoURL`, NULL-guarded) and later allocations reuse it.
   `UpsertRegistryTask` gained a `repo_url` param, COALESCEd so a caller that
   does not know the repo never erases it.
3. **Idempotency key = `run_id`.** `worktrees.run_id UNIQUE`; a retried
   allocate returns the existing row with `existing=true`. Worktree dir is the
   deterministic `<WORKTREE_ROOT>/<run_id>` so crash-orphan dirs are
   identifiable by reconcile.
4. **One allocated worktree per task** enforced by partial unique index
   `idx_worktrees_active_per_task` (mirrors I-03 for runs). Unreachable via
   the service path (I-03 already caps active runs) but guarded anyway.
5. **Dirt ⇒ quarantine, never delete.** Any uncommitted state — tracked or
   untracked — moves the dir to `WORKTREE_QUARANTINE_ROOT` and flips status to
   `quarantined`. `git worktree remove` only runs on a clean tree; `--force`
   is never used.
6. **Reconcile runs twice ways:** once at worker boot (crash orphans, expired
   leases left by the dead process) and on Temporal schedule
   `vtx-worktree-reconcile` every `WORKTREE_RECONCILE_INTERVAL` (lease expiry
   happens when *other* workers die, so a boot sweep alone cannot cover it).
7. **Paths are canonicalized** against the configured roots before any fs/git
   op (`gitops.CanonicalPath`): traversal and symlink escapes are rejected.

## Surfaces

- **Temporal activities** (registered in `cmd/worker`):
  `AllocateWorktree`, `ReleaseWorktree`, `ReconcileWorktrees`;
  `WorktreeReconcileWorkflow` is the schedule entrypoint.
- **HTTP** (`RegistryActorMiddleware` group):
  `POST /api/v1/worktrees/allocate` `{run_id, repo_url?, base_ref?}` → 201/200,
  `GET /api/v1/worktrees`, `GET /api/v1/worktrees/runs/{run_id}`,
  `POST /api/v1/worktrees/runs/{run_id}/release` `{reason?}`.

## Config

| Env | Default | Meaning |
|---|---|---|
| `WORKTREE_ROOT` | `./var/worktrees` | one child dir per run |
| `WORKTREE_QUARANTINE_ROOT` | `./var/worktrees-quarantine` | dirty/orphan destination |
| `REPO_CACHE_ROOT` | `./var/repo-cache` | one clone per `repo_url` |
| `WORKTREE_LEASE_TTL` | `2m` | lease expiry written on acquire |
| `WORKTREE_RECONCILE_INTERVAL` | `5m` | Temporal schedule interval |
| `WORKTREE_OWNER` | `worktree-service` | `task_runs.lease_owner` written by this process |

## Operator notes

- Reconcile report is logged as `worktree_reconcile` with counts; entries in
  `errors[]` are per-item failures (a dirty dir it could not move, etc.) — the
  pass continues past them.
- Quarantined dirs are never auto-deleted; operators inspect and remove
  manually.
- `worktrees` rows are kept after release (history), like `task_run_events`.

## Handoff

- **VTX-5**: call `AllocateWorktree` (activity or `POST /worktrees/allocate`)
  inside the `preparing` stage; pass `repo_url` on the first allocation for a
  task. `existing=true` means a previous attempt already owns the row.
- **VTX-7**: run the agent with `cwd = response.path`, branch already created
  (`response.branch`). On run completion call `ReleaseWorktree` — dirty trees
  quarantine, so uncommitted agent output is never lost silently.

## Remaining risks

- First-time `git clone` of a large repo runs inside the allocation tx (row
  lock held, same-run callers blocked). Acceptable at current scale; move to a
  pre-stage if clone latency becomes visible.
- `git worktree remove` requires the repo cache clone to exist — removal of a
  worktree whose cache was wiped will fail until the clone is restored.
- The Temporal schedule is created by whichever worker boots first; deleting
  it requires `tctl`/UI — there is no admin surface.
