---
title: vtx-4 worktree review fixes
date: 2026-10-22
summary: "Repaired 4 review findings: lease renewal wired, missing worktree root, leaf-symlink escape, stale git metadata on quarantine"
---

# vtx-4 worktree review fixes

## What happened
Applied fixes for the four findings from the VTX-4 review (dev @ e98217c):

1. **Lease never renewed (P1).** `RenewRunLease` existed in sqlc but had no caller. Added `WorktreeStore.RenewRunLease` to the port, `PgWorktreeStore.RenewRunLease` (pgx.ErrNoRows → ErrWorktreeLeaseLost), `WorktreeService.RenewLease(ctx, runID, epoch)` fencing on owner+epoch, and a `RenewWorktreeLease` Temporal activity that records a heartbeat.
2. **Fresh deploys could not allocate (P1).** `Runner.Add` now `MkdirAll`s `WorktreeRoot` before canonicalization; `CanonicalPath` itself was rewritten to resolve the deepest *existing* ancestor and re-append the non-existent suffix, so missing roots no longer fail `EvalSymlinks`.
3. **Leaf symlink bypass (P1).** `CanonicalPath` now `Lstat`s the leaf and rejects it when it exists as a symlink; the new resolve-existing-prefix walk inspects every existing component.
4. **Quarantine left stale git metadata (P2).** `Quarantine` still renames the dir first (files preserved), then runs `git worktree repair <dest>` against the main repo derived from the worktree's `.git` pointer file; on repair failure it falls back to `git worktree prune` to unregister the stale entry. Non-git dirs (orphans) take the plain rename path.

## Decision
Renewal is exposed as a standalone activity (`RenewWorktreeLease`) rather than embedded in AllocateWorktree — the execution workflow (VTX-7) schedules it before `LeaseExpiry`. Quarantine prefers `repair` (keeps the moved worktree consistent) with `prune` as the unregistering fallback.

## Verification
- `make test-boundaries`, `make vet`, `make typecheck`, `make test`, `make build` — all pass.
- New regression tests: leaf-symlink rejection, missing-root canonicalization, renew success/stale-epoch, and `git worktree list` no longer containing the old path after quarantine (real-git lifecycle test).
- DB-backed worktree integration tests remain skipped locally (no TEST_DATABASE_URL postgres).

## Next steps
None for this fix. VTX-7's execution activity must call `RenewWorktreeLease` with the `LeaseEpoch` returned by `AllocateWorktree` before `cfg.WorktreeLeaseTTL` elapses.

> Historical work record — not durable authority. Prefer docs/specs/ADRs for current decisions.
