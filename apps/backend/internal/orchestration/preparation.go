package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/requirement"
	"go.temporal.io/sdk/workflow"
)

// PreparationActivities executes the VTX-6 side of the preparing stage; no
// agent dispatch occurs until both real pieces of evidence have committed.
type PreparationActivities struct {
	Worktrees    *application.WorktreeService
	Requirements *requirement.Service
	Registry     *application.RegistryService
	Reader       application.RegistryReader
}

func (a *PreparationActivities) PrepareRun(ctx context.Context, in AllocateWorktreeInput) (requirement.AgentDispatch, error) {
	if _, err := a.Worktrees.AllocateWorktree(ctx, application.AllocateCommand{RunID: in.RunID, RepoURL: in.RepoURL, BaseRef: in.BaseRef}); err != nil {
		return requirement.AgentDispatch{}, fmt.Errorf("prepare worktree: %w", err)
	}
	if _, err := a.Requirements.Extract(ctx, in.RunID); err != nil {
		return requirement.AgentDispatch{}, err
	}
	if _, err := a.Requirements.RecordBaseline(ctx, in.RunID); err != nil {
		return requirement.AgentDispatch{}, err
	}
	dispatch, err := a.Requirements.Dispatch(ctx, in.RunID)
	if err != nil {
		return dispatch, err
	}
	run, err := a.Reader.GetRun(ctx, in.RunID)
	if err != nil {
		return dispatch, err
	}
	metadata, err := json.Marshal(map[string]string{"requirement_hash": dispatch.Requirement.RequirementHash})
	if err != nil {
		return dispatch, err
	}
	_, err = a.Registry.TransitionRun(ctx, application.TransitionCommand{RunID: in.RunID, ExpectedVersion: run.Version, ExpectedLeaseEpoch: run.LeaseEpoch, Action: domain.ActionAdvance, Actor: domain.ActorOrchestrator, ActorType: domain.ActorTypeSystem, Metadata: metadata})
	return dispatch, err
}
func PreparationWorkflow(ctx workflow.Context, in AllocateWorktreeInput) (requirement.AgentDispatch, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 20 * time.Minute})
	var out requirement.AgentDispatch
	err := workflow.ExecuteActivity(ctx, "PrepareRun", in).Get(ctx, &out)
	return out, err
}
