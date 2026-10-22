package application

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Worktree statuses (mirror the CHECK constraint in 00005_worktrees.sql).
const (
	WorktreeAllocated   = "allocated"
	WorktreeReleased    = "released"
	WorktreeQuarantined = "quarantined"
)

// DefaultLeaseTTL bounds one allocation's claim on the run. The Temporal
// activity renews via heartbeats→RenewRunLease; expiry is what lets the
// reconciler reclaim worktrees whose worker died.
const DefaultLeaseTTL = 2 * time.Minute

// WorktreeService is the only worktree lifecycle authority (VTX-4). VTX-3
// remains the run/state authority — this service acquires the run lease VTX-3
// defined but never wrote.
type WorktreeService struct {
	store    WorktreeStore
	git      WorktreeGit
	owner    string
	leaseTTL time.Duration
	clock    Clock
	logger   *slog.Logger
}

// NewWorktreeService builds the service. owner defaults to a process identity;
// VTX-5 passes its scheduler owner so the run lease names the true holder.
func NewWorktreeService(store WorktreeStore, git WorktreeGit, owner string, leaseTTL time.Duration, clock Clock, logger *slog.Logger) *WorktreeService {
	if owner == "" {
		owner = "worktree-service"
	}
	if leaseTTL <= 0 {
		leaseTTL = DefaultLeaseTTL
	}
	if clock == nil {
		clock = SystemClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &WorktreeService{store: store, git: git, owner: owner, leaseTTL: leaseTTL, clock: clock, logger: logger}
}

// AllocateWorktree gives one run its worktree: run lock → terminal check →
// run-lease acquire (epoch bump is the fencing token) → repo cache + git add →
// row + task_runs.branch, all inside one tx. Same-run retry returns the
// existing row with Existing=true.
func (s *WorktreeService) AllocateWorktree(ctx context.Context, cmd AllocateCommand) (Allocation, error) {
	if cmd.RunID == "" {
		return Allocation{}, fmt.Errorf("%w: run_id required", ErrPayloadInvalid)
	}
	cmd.Owner = s.owner
	cmd.LeaseExpiresAt = s.clock.Now().Add(s.leaseTTL)

	// The effect runs INSIDE the allocation tx so a git failure rolls back the
	// lease and the row together. The run row lock it holds only blocks
	// same-run allocators — exactly the serialization we want. A first-time
	// clone can be slow; the crash-orphan sweep covers the mid-clone window.
	effect := func(in AllocateInput) (path, branch, baseSHA string, err error) {
		repoURL := in.RepoURL
		if repoURL == "" {
			repoURL = cmd.RepoURL
		}
		if repoURL == "" {
			return "", "", "", ErrWorktreeNoRepo
		}
		repoDir, err := s.git.EnsureRepo(ctx, repoURL)
		if err != nil {
			return "", "", "", fmt.Errorf("ensure repo cache: %w", err)
		}
		branch = BranchFor(in.JiraIssueKey, in.RunID)
		baseRef := cmd.BaseRef
		if baseRef == "" {
			baseRef = "HEAD"
		}
		path, err = s.git.Add(ctx, repoDir, in.RunID, branch, baseRef)
		if err != nil {
			return "", "", "", err
		}
		baseSHA, err = s.git.HeadSHA(ctx, path)
		if err != nil {
			s.logger.Warn("worktree base sha unresolved; continuing without it", "run_id", in.RunID, "error", err)
			baseSHA = ""
		}
		return path, branch, baseSHA, nil
	}

	alloc, err := s.store.AllocateWorktree(ctx, cmd, effect)
	if err != nil {
		return Allocation{}, err
	}
	s.logger.Info("worktree allocated",
		"metric", "worktree_allocated",
		"run_id", alloc.Worktree.RunID,
		"task_id", alloc.Worktree.TaskID,
		"path", alloc.Worktree.Path,
		"branch", alloc.Worktree.Branch,
		"lease_epoch", alloc.LeaseEpoch,
		"existing", alloc.Existing,
	)
	return alloc, nil
}

// LeaseExpiry is the TTL callers use to schedule renewals.
func (s *WorktreeService) LeaseExpiry() time.Duration { return s.leaseTTL }

// ReleaseWorktree runs the cleanup path for one run (run finished or its lease
// was revoked). Dirt of any kind quarantines; only clean worktrees are removed.
func (s *WorktreeService) ReleaseWorktree(ctx context.Context, runID, reason string) (WorktreeView, error) {
	wt, err := s.store.GetWorktree(ctx, runID)
	if err != nil {
		return WorktreeView{}, err
	}
	if wt.Status != WorktreeAllocated {
		return wt, nil // already released/quarantined: idempotent
	}
	return s.cleanupOne(ctx, wt, reason)
}

// Reconcile scans DB truth vs disk truth. Boot sweep and the Temporal schedule
// call the same path (advise decision 4). A row is reclaimable when its run is
// terminal or its lease expired — a live lease is never touched.
func (s *WorktreeService) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var rep ReconcileReport
	rows, err := s.store.ListAllocatedWorktrees(ctx)
	if err != nil {
		return rep, fmt.Errorf("list allocated worktrees: %w", err)
	}
	known := map[string]bool{}
	now := s.clock.Now()
	for _, row := range rows {
		known[row.RunID] = true
		terminal := row.RunState == "completed" || row.RunState == "failed" || row.RunState == "cancelled"
		expired := row.RunLeaseExpireAt == nil || row.RunLeaseExpireAt.Before(now)
		if !terminal && !expired {
			continue
		}
		reason := "run terminal"
		if !terminal {
			reason = "run lease expired"
		}
		outcome, err := s.cleanupAllocated(ctx, row, reason)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("run %s: %v", row.RunID, err))
			continue
		}
		switch outcome {
		case WorktreeReleased:
			rep.Released++
		case WorktreeQuarantined:
			rep.Quarantined++
		}
	}

	orphans, err := s.git.OrphanDirs(ctx, known)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("disk sweep: %v", err))
		return rep, nil
	}
	for _, dir := range orphans {
		if _, err := s.git.Quarantine(ctx, filepath.Base(dir), dir, "orphan dir without worktree row"); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("orphan %s: %v", dir, err))
			continue
		}
		rep.OrphansQuarantined++
	}
	s.logger.Info("worktree reconcile",
		"metric", "worktree_reconcile",
		"released", rep.Released,
		"quarantined", rep.Quarantined,
		"orphans_quarantined", rep.OrphansQuarantined,
		"errors", len(rep.Errors),
	)
	return rep, nil
}

// ReconcileReport is the observable outcome of one reconcile pass.
type ReconcileReport struct {
	Released           int
	Quarantined        int
	OrphansQuarantined int
	Errors             []string
}

func (s *WorktreeService) cleanupAllocated(ctx context.Context, row AllocatedWorktreeView, reason string) (string, error) {
	return s.cleanup(ctx, row.WorktreeView, row.RepoURL, reason)
}

func (s *WorktreeService) cleanupOne(ctx context.Context, wt WorktreeView, reason string) (WorktreeView, error) {
	status, err := s.cleanup(ctx, wt, wt.RepoURL, reason)
	if err != nil {
		return WorktreeView{}, err
	}
	_ = status
	return s.store.GetWorktree(ctx, wt.RunID)
}

// cleanup is the shared dirty→quarantine / clean→remove decision. The status
// flip is a guarded write so a racing reconciler cannot double-claim.
func (s *WorktreeService) cleanup(ctx context.Context, wt WorktreeView, repoURL, reason string) (string, error) {
	dirty, err := s.git.IsDirty(ctx, wt.Path)
	if err != nil {
		// Dir already gone (deleted out-of-band or a partially-failed earlier
		// cleanup): nothing to reclaim — just close the row out.
		if _, statErr := os.Stat(wt.Path); os.IsNotExist(statErr) {
			if _, markErr := s.store.CompleteCleanup(ctx, wt.WorktreeID, WorktreeAllocated, WorktreeReleased, "path already absent"); markErr != nil {
				return "", fmt.Errorf("mark released for absent path: %w", markErr)
			}
			return WorktreeReleased, nil
		}
		return "", fmt.Errorf("check dirty state: %w", err)
	}
	if dirty {
		if _, err := s.git.Quarantine(ctx, wt.RunID, wt.Path, reason); err != nil {
			return "", fmt.Errorf("quarantine worktree: %w", err)
		}
		if _, err := s.store.CompleteCleanup(ctx, wt.WorktreeID, WorktreeAllocated, WorktreeQuarantined, reason); err != nil {
			return "", fmt.Errorf("mark quarantined: %w", err)
		}
		return WorktreeQuarantined, nil
	}
	repoDir, err := s.git.EnsureRepo(ctx, repoURL)
	if err != nil {
		return "", fmt.Errorf("resolve repo for removal: %w", err)
	}
	if err := s.git.Remove(ctx, repoDir, wt.Path); err != nil {
		return "", err
	}
	if _, err := s.store.CompleteCleanup(ctx, wt.WorktreeID, WorktreeAllocated, WorktreeReleased, reason); err != nil {
		return "", fmt.Errorf("mark released: %w", err)
	}
	return WorktreeReleased, nil
}

// GetWorktree exposes the row for reads/HTTP.
func (s *WorktreeService) GetWorktree(ctx context.Context, runID string) (WorktreeView, error) {
	return s.store.GetWorktree(ctx, runID)
}

// ListAllocated exposes the reconcile read model (HTTP/status surfaces).
func (s *WorktreeService) ListAllocated(ctx context.Context) ([]AllocatedWorktreeView, error) {
	return s.store.ListAllocatedWorktrees(ctx)
}

// BranchFor renders the collision-safe branch name: vtx/<issue_key>/<run8>.
// Anything git-ref-unsafe collapses to '-'. Written to task_runs.branch (the
// VTX-3 column) so run history and the worktree row name the same ref.
func BranchFor(issueKey, runID string) string {
	clean := func(s string) string {
		var b strings.Builder
		prevDash := false
		for _, r := range s {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				if !prevDash {
					b.WriteByte('-')
					prevDash = true
				}
				continue
			}
			b.WriteRune(r)
			prevDash = false
		}
		return strings.Trim(b.String(), "-")
	}
	short := runID
	if len(short) > 8 {
		short = short[:8]
	}
	issue := clean(issueKey)
	if issue == "" {
		issue = "task"
	}
	return "vtx/" + issue + "/" + clean(short)
}
