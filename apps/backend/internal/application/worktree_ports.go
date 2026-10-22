package application

import (
	"context"
	"errors"
	"time"
)

// VTX-4 worktree ports. Same discipline as the registry: exactly one
// production implementation per port (pgx store, git runner) and a fake in
// tests. The store owns transactions; the git port owns the filesystem.

var (
	// ErrWorktreeRunTerminal — the run reached a terminal state; no worktree
	// may be allocated for it.
	ErrWorktreeRunTerminal = errors.New("worktree: run is terminal")
	// ErrWorktreeNoRepo — the task has no repo_url and the caller supplied none.
	ErrWorktreeNoRepo = errors.New("worktree: task has no repo_url")
	// ErrWorktreeLeaseLost — a stale epoch was presented, or the lease is held
	// by another owner.
	ErrWorktreeLeaseLost = errors.New("worktree: lease lost")
	// ErrWorktreeNotFound — no worktree row for the run.
	ErrWorktreeNotFound = errors.New("worktree: worktree not found")
	// ErrWorktreeTaskBusy — the task already has an allocated worktree owned
	// by a different run (partial unique index violation).
	ErrWorktreeTaskBusy = errors.New("worktree: task already has an allocated worktree")
)

// AllocateCommand is one allocation request. RunID identifies the run; Owner
// is the identity written to task_runs.lease_owner (advise decision 1: VTX-4
// acquires the run lease itself). RepoURL is optional: it binds the task's
// repo on first allocation and is ignored once registry_tasks.repo_url is set.
type AllocateCommand struct {
	RunID   string
	Owner   string
	RepoURL string
	BaseRef string
	// LeaseExpiresAt is computed by the service (its clock + TTL) so expiry is
	// testable; the store just writes it.
	LeaseExpiresAt time.Time
}

// Allocation is the result of AllocateWorktree: the durable row plus the lease
// epoch the caller must present on later lease-gated operations.
type Allocation struct {
	Worktree   WorktreeView
	LeaseOwner string
	LeaseEpoch int64
	// Existing is true when the row already belonged to this run (idempotent
	// retry): the caller must NOT treat it as a fresh allocation.
	Existing bool
}

// AllocateInput is what the service sees of the locked run inside the tx.
type AllocateInput struct {
	RunID        string
	TaskID       string
	State        string
	RepoURL      string // resolved: task.repo_url or cmd.RepoURL
	JiraIssueKey string
}

// WorktreeView is a worktrees row.
type WorktreeView struct {
	WorktreeID       string
	RunID            string
	TaskID           string
	Status           string
	Path             string
	Branch           string
	RepoURL          string
	BaseSHA          *string
	QuarantineReason *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ReleasedAt       *time.Time
}

// AllocatedWorktreeView joins an allocated row to its run for reconcile reads.
type AllocatedWorktreeView struct {
	WorktreeView
	RunState         string
	RunLeaseOwner    *string
	RunLeaseEpoch    int64
	RunLeaseExpireAt *time.Time
}

// WorktreeStore is the transactional persistence surface for worktrees.
type WorktreeStore interface {
	// AllocateWorktree runs the allocation transaction: the run row is locked
	// FOR UPDATE, terminal state is rejected, the run lease is acquired
	// (epoch bumped), effect performs the on-disk work, then the worktree row
	// and task_runs.branch are written. Any effect error rolls the whole tx
	// back — a run either has (lease + dir + row + branch) or none of them.
	// An existing row for the run short-circuits everything (idempotent).
	AllocateWorktree(ctx context.Context, cmd AllocateCommand, effect func(AllocateInput) (path, branch, baseSHA string, err error)) (Allocation, error)

	// GetWorktree reads the row for a run (any status).
	GetWorktree(ctx context.Context, runID string) (WorktreeView, error)

	// CompleteCleanup guards a status transition: it only applies when the row
	// is still in fromStatus, so a reconciler racing a release cannot both
	// claim the worktree.
	CompleteCleanup(ctx context.Context, worktreeID, fromStatus, toStatus, reason string) (WorktreeView, error)

	// ListAllocatedWorktrees returns every allocated row joined to its run.
	ListAllocatedWorktrees(ctx context.Context) ([]AllocatedWorktreeView, error)

	// RunLeaseState reads the run's current lease fields and state.
	RunLeaseState(ctx context.Context, runID string) (owner *string, epoch int64, expiresAt *time.Time, state string, err error)

	// RenewRunLease extends the lease for owner+epoch; returns the new expiry.
	// ErrWorktreeLeaseLost when the presented epoch is stale, the owner does
	// not match, the lease already expired, or the run is terminal.
	RenewRunLease(ctx context.Context, runID, owner string, epoch int64, expiresAt time.Time) (time.Time, error)

	// ReleaseRunLease clears the lease for owner+epoch (idempotent: a stale
	// owner can never clear a fresher lease).
	ReleaseRunLease(ctx context.Context, runID, owner string, epoch int64) error
}

// WorktreeGit is the disk side of the lifecycle (internal/gitops implements
// it). All paths the service passes are already canonicalized.
type WorktreeGit interface {
	// EnsureRepo clones or fetches the repo cache for repoURL; returns the
	// cache dir used as the worktree parent.
	EnsureRepo(ctx context.Context, repoURL string) (string, error)
	// Add creates the worktree for runID on branch from baseRef; returns the
	// canonical path. Idempotent across crashes for the same runID.
	Add(ctx context.Context, repoDir, runID, branch, baseRef string) (string, error)
	// HeadSHA resolves HEAD of the worktree at path.
	HeadSHA(ctx context.Context, path string) (string, error)
	// IsDirty reports any uncommitted state (tracked or untracked).
	IsDirty(ctx context.Context, path string) (bool, error)
	// Remove deletes a CLEAN worktree. Never force-removes.
	Remove(ctx context.Context, repoDir, path string) error
	// Quarantine moves the worktree dir to the quarantine root; returns dest.
	Quarantine(ctx context.Context, runID, path, reason string) (string, error)
	// OrphanDirs lists full paths of run-id-named dirs under the worktree root
	// whose base names are not in known — crash-window orphans with no DB row.
	OrphanDirs(ctx context.Context, known map[string]bool) ([]string, error)
}
