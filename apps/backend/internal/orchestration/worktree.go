package orchestration

import (
	"context"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"

	"github.com/vtx/platform/internal/application"
)

// VTX-4 worktree activities. All I/O lives here (Temporal determinism rule):
// workflows only schedule, never allocate.

// WorktreeActivities holds the service handle the worker registers.
type WorktreeActivities struct {
	svc *application.WorktreeService
}

func NewWorktreeActivities(svc *application.WorktreeService) *WorktreeActivities {
	return &WorktreeActivities{svc: svc}
}

// AllocateWorktreeInput is the preparing activity's request.
type AllocateWorktreeInput struct {
	RunID   string `json:"run_id"`
	RepoURL string `json:"repo_url,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
}

// AllocateWorktreeOutput hands VTX-7 everything it needs to execute in the tree.
type AllocateWorktreeOutput struct {
	WorktreeID string `json:"worktree_id"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	RepoURL    string `json:"repo_url"`
	LeaseEpoch int64  `json:"lease_epoch"`
	Existing   bool   `json:"existing"`
}

// AllocateWorktree is idempotent per run: a Temporal activity retry returns
// the same durable row.
func (a *WorktreeActivities) AllocateWorktree(ctx context.Context, in AllocateWorktreeInput) (AllocateWorktreeOutput, error) {
	alloc, err := a.svc.AllocateWorktree(ctx, application.AllocateCommand{
		RunID:   in.RunID,
		RepoURL: in.RepoURL,
		BaseRef: in.BaseRef,
	})
	if err != nil {
		return AllocateWorktreeOutput{}, err
	}
	return AllocateWorktreeOutput{
		WorktreeID: alloc.Worktree.WorktreeID,
		Path:       alloc.Worktree.Path,
		Branch:     alloc.Worktree.Branch,
		RepoURL:    alloc.Worktree.RepoURL,
		LeaseEpoch: alloc.LeaseEpoch,
		Existing:   alloc.Existing,
	}, nil
}

// RenewWorktreeLeaseInput carries the fencing epoch the allocator returned.
type RenewWorktreeLeaseInput struct {
	RunID      string `json:"run_id"`
	LeaseEpoch int64  `json:"lease_epoch"`
}

// RenewWorktreeLease is the heartbeat→renewal the lease contract requires.
// Until the VTX-7 execution workflow lands and schedules it, NO caller exists:
// the reconciler therefore only reclaims terminal runs (worktree_service.go
// documents the interim rule) instead of trusting leases that can never be
// renewed.
func (a *WorktreeActivities) RenewWorktreeLease(ctx context.Context, in RenewWorktreeLeaseInput) (time.Time, error) {
	activity.RecordHeartbeat(ctx, "lease-renew")
	return a.svc.RenewLease(ctx, in.RunID, in.LeaseEpoch)
}

// ReleaseWorktreeInput asks the service to clean up a finished run's tree.
type ReleaseWorktreeInput struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason,omitempty"`
}

// ReleaseWorktree never deletes a dirty tree: dirt quarantines instead.
func (a *WorktreeActivities) ReleaseWorktree(ctx context.Context, in ReleaseWorktreeInput) error {
	_, err := a.svc.ReleaseWorktree(ctx, in.RunID, in.Reason)
	return err
}

// ReconcileWorktrees is the periodic/boot reconcile pass (crash-safe recovery).
func (a *WorktreeActivities) ReconcileWorktrees(ctx context.Context) (application.ReconcileReport, error) {
	activity.GetLogger(ctx).Info("worktree reconcile pass")
	return a.svc.Reconcile(ctx)
}

// WorktreeReconcileWorkflow is the schedule's entrypoint: one activity call,
// nothing else — all logic stays in the activity for testability.
func WorktreeReconcileWorkflow(ctx workflow.Context) (application.ReconcileReport, error) {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	var rep application.ReconcileReport
	err := workflow.ExecuteActivity(ctx, "ReconcileWorktrees").Get(ctx, &rep)
	return rep, err
}
