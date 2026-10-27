package spineserver

import (
	"context"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/gate"
	"github.com/vtx/platform/internal/qa"
)

// Ports are the narrow capabilities the spine depends on. Each is satisfied by
// a service that already exists; the spine server holds them as interfaces so it
// stays unit-testable with fakes and so a missing dependency degrades to
// tool_unavailable instead of a startup crash. A nil port disables its tool.

// Intake creates an execution intent from a polled Jira issue (agent master).
// It is the only tool that is not run-scoped: no run exists yet. The service
// re-fetches Jira and owns dedupe; the caller's payload is never authoritative.
type Intake interface {
	Intake(ctx context.Context, req application.PollIntakeRequest) (application.PollIntakeResult, error)
}

// Verifier runs the build/test suites and records evidence (VTX-8).
type Verifier interface {
	VerifyRun(ctx context.Context, cmd application.VerifyCommand) (application.VerifyOutcome, error)
}

// GateEvaluator recomputes the quality-gate decision from committed evidence.
// The agent supplies only run_id + lease_epoch; the verdict is never an input.
type GateEvaluator interface {
	Evaluate(ctx context.Context, runID string, leaseEpoch int64) (gate.Decision, error)
}

// QA launches and records the independent, exact-SHA QA session (VTX-9).
type QA interface {
	LaunchSession(ctx context.Context, cmd qa.LaunchCommand) (qa.Session, error)
	SubmitVerdict(ctx context.Context, cmd qa.SubmitCommand) (qa.VerdictRecord, error)
}

// PROpener creates the Draft PR, refusing unless the gate already passed (VTX-12).
type PROpener interface {
	OpenDraftPR(ctx context.Context, cmd application.OpenPRCommand) (application.PullRequestView, error)
}

// Transitioner is the single writer of run state (VTX-3): version CAS + lease
// fencing + atomic audit/outbox.
type Transitioner interface {
	TransitionRun(ctx context.Context, cmd application.TransitionCommand) (application.RunSnapshot, error)
}

// LeaseRenewer extends the run's worktree lease (VTX-4); the extension calls it
// on a heartbeat so a long orchestration run never loses its fence.
type LeaseRenewer interface {
	RenewLease(ctx context.Context, runID string, epoch int64) (time.Time, error)
}
