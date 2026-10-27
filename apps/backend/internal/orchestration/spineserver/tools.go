package spineserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/qa"
)

// route dispatches an authenticated request to its tool handler. A nil port
// makes the tool unavailable rather than panicking; the extension treats
// tool_unavailable as "spine not ready for this stage" and fails closed.
func (s *Server) route(ctx context.Context, req Request) Response {
	switch req.Tool {
	case ToolIntake:
		return s.toolIntake(ctx, req)
	case ToolVerify:
		return s.toolVerify(ctx, req)
	case ToolGate:
		return s.toolGate(ctx, req)
	case ToolQALaunch:
		return s.toolQALaunch(ctx, req)
	case ToolQAVerdict:
		return s.toolQAVerdict(ctx, req)
	case ToolOpenPR:
		return s.toolOpenPR(ctx, req)
	case ToolAdvance:
		return s.toolTransition(ctx, req, domain.ActionAdvance, domain.ActorOrchestrator)
	case ToolBlock:
		// Only operator/policy may block (domain.transitionTable); the spine
		// blocks as the automated policy actor, never as the orchestrator.
		return s.toolTransition(ctx, req, domain.ActionBlock, domain.ActorPolicy)
	case ToolRenewLease:
		return s.toolRenewLease(ctx, req)
	default:
		return errResponse(req.ID, req.Tool, ErrCodeUnknownTool, fmt.Sprintf("unknown tool %q", req.Tool))
	}
}

// decodeArgs unmarshals req.Args into dst. An empty Args is treated as an empty
// object so tools with no arguments still work.
func (s *Server) decodeArgs(req Request, dst any) *ResponseError {
	raw := req.Args
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return &ResponseError{Code: ErrCodeInvalidArgs, Message: "invalid args: " + err.Error()}
	}
	return nil
}

// toolIntake is the poller→pipeline bridge. It is not run-scoped: the service
// re-fetches Jira (identity + status are authority-checked) and persists the
// intent + outbox event idempotently per issue version.
func (s *Server) toolIntake(ctx context.Context, req Request) Response {
	if s.ports.Intake == nil {
		return errResponse(req.ID, req.Tool, ErrCodeToolUnavailable, "poll intake not configured")
	}
	var args application.PollIntakeRequest
	if e := s.decodeArgs(req, &args); e != nil {
		return Response{ID: req.ID, Tool: req.Tool, Error: e}
	}
	out, err := s.ports.Intake.Intake(ctx, args)
	if err != nil {
		return s.serviceError(req, err)
	}
	return okResponse(req.ID, req.Tool, out)
}

func (s *Server) toolVerify(ctx context.Context, req Request) Response {
	if s.ports.Verifier == nil {
		return errResponse(req.ID, req.Tool, ErrCodeToolUnavailable, "verification service not configured")
	}
	var args struct {
		TaskID  string `json:"task_id"`
		Attempt int32  `json:"attempt"`
	}
	if e := s.decodeArgs(req, &args); e != nil {
		return Response{ID: req.ID, Tool: req.Tool, Error: e}
	}
	cmd := application.VerifyCommand{
		RunID:      req.RunID,
		TaskID:     args.TaskID,
		Attempt:    args.Attempt,
		LeaseEpoch: req.LeaseEpoch,
		LeaseOwner: s.cfg.LeaseOwner,
	}
	out, err := s.ports.Verifier.VerifyRun(ctx, cmd)
	if err != nil {
		return s.serviceError(req, err)
	}
	return okResponse(req.ID, req.Tool, out)
}

func (s *Server) toolGate(ctx context.Context, req Request) Response {
	if s.ports.Gate == nil {
		return errResponse(req.ID, req.Tool, ErrCodeToolUnavailable, "gate service not configured")
	}
	// The agent supplies only the lease epoch; the decision is recomputed from
	// committed evidence, never taken from the request (FR-QA-002).
	var args struct {
		LeaseEpoch int64 `json:"lease_epoch"`
	}
	if e := s.decodeArgs(req, &args); e != nil {
		return Response{ID: req.ID, Tool: req.Tool, Error: e}
	}
	epoch := req.LeaseEpoch
	if args.LeaseEpoch != 0 {
		epoch = args.LeaseEpoch
	}
	decision, err := s.ports.Gate.Evaluate(ctx, req.RunID, epoch)
	if err != nil {
		return s.serviceError(req, err)
	}
	return okResponse(req.ID, req.Tool, decision)
}

func (s *Server) toolQALaunch(ctx context.Context, req Request) Response {
	if s.ports.QA == nil {
		return errResponse(req.ID, req.Tool, ErrCodeToolUnavailable, "qa service not configured")
	}
	var cmd qa.LaunchCommand
	if e := s.decodeArgs(req, &cmd); e != nil {
		return Response{ID: req.ID, Tool: req.Tool, Error: e}
	}
	cmd.RunID = req.RunID
	if cmd.LeaseEpoch == 0 {
		cmd.LeaseEpoch = req.LeaseEpoch
	}
	if cmd.LeaseOwner == "" {
		cmd.LeaseOwner = s.cfg.LeaseOwner
	}
	session, err := s.ports.QA.LaunchSession(ctx, cmd)
	if err != nil {
		return s.serviceError(req, err)
	}
	return okResponse(req.ID, req.Tool, session)
}

func (s *Server) toolQAVerdict(ctx context.Context, req Request) Response {
	if s.ports.QA == nil {
		return errResponse(req.ID, req.Tool, ErrCodeToolUnavailable, "qa service not configured")
	}
	var cmd qa.SubmitCommand
	if e := s.decodeArgs(req, &cmd); e != nil {
		return Response{ID: req.ID, Tool: req.Tool, Error: e}
	}
	cmd.RunID = req.RunID
	// Identity is fixed server-side; the agent cannot claim to be qa-agent.
	cmd.QAIdentity = "qa-agent"
	if cmd.LeaseEpoch == 0 {
		cmd.LeaseEpoch = req.LeaseEpoch
	}
	if cmd.LeaseOwner == "" {
		cmd.LeaseOwner = s.cfg.LeaseOwner
	}
	record, err := s.ports.QA.SubmitVerdict(ctx, cmd)
	if err != nil {
		return s.serviceError(req, err)
	}
	return okResponse(req.ID, req.Tool, record)
}

func (s *Server) toolOpenPR(ctx context.Context, req Request) Response {
	if s.ports.PRs == nil {
		return errResponse(req.ID, req.Tool, ErrCodeToolUnavailable, "pull request service not configured")
	}
	var args struct {
		ExpectedVersion int64  `json:"expected_version"`
		Title           string `json:"title"`
		Body            string `json:"body"`
	}
	if e := s.decodeArgs(req, &args); e != nil {
		return Response{ID: req.ID, Tool: req.Tool, Error: e}
	}
	// run_id comes from the fenced request, never from args: an agent cannot
	// redirect a PR onto a different run by lying in the payload.
	cmd := application.OpenPRCommand{
		RunID:              req.RunID,
		ExpectedVersion:    args.ExpectedVersion,
		ExpectedLeaseEpoch: req.LeaseEpoch,
		Title:              args.Title,
		Body:               args.Body,
	}
	view, err := s.ports.PRs.OpenDraftPR(ctx, cmd)
	if err != nil {
		return s.serviceError(req, err)
	}
	return okResponse(req.ID, req.Tool, view)
}

// toolTransition drives one state move through the single registry writer. The
// spine fixes the actor and actor type: the agent chooses the action and passes
// the CAS version, but never impersonates a human or operator principal.
func (s *Server) toolTransition(ctx context.Context, req Request, action domain.Action, actor domain.Actor) Response {
	if s.ports.Transitions == nil {
		return errResponse(req.ID, req.Tool, ErrCodeToolUnavailable, "registry transition service not configured")
	}
	var args struct {
		ExpectedVersion int64           `json:"expected_version"`
		Reason          string          `json:"reason"`
		Metadata        json.RawMessage `json:"metadata"`
	}
	if e := s.decodeArgs(req, &args); e != nil {
		return Response{ID: req.ID, Tool: req.Tool, Error: e}
	}
	cmd := application.TransitionCommand{
		RunID:              req.RunID,
		ExpectedVersion:    args.ExpectedVersion,
		ExpectedLeaseEpoch: req.LeaseEpoch,
		Action:             action,
		Actor:              actor,
		ActorType:          domain.ActorTypeSystem,
		Reason:             args.Reason,
		Metadata:           args.Metadata,
	}
	snap, err := s.ports.Transitions.TransitionRun(ctx, cmd)
	if err != nil {
		return s.serviceError(req, err)
	}
	return okResponse(req.ID, req.Tool, runViewOf(snap))
}

// runView is the tool-protocol shape of a run snapshot. The extension tracks
// version/state from these lowercase keys to fence its next CAS; encoding the
// application struct directly would emit Go field names and leave the client
// stuck on the launch-time version.
type runView struct {
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	Version    int64  `json:"version"`
	Attempt    int32  `json:"attempt"`
	LeaseEpoch int64  `json:"lease_epoch"`
	CommitSHA  string `json:"commit_sha,omitempty"`
}

func runViewOf(s application.RunSnapshot) runView {
	view := runView{
		RunID:      s.RunID,
		State:      string(s.State),
		Version:    s.Version,
		Attempt:    s.Attempt,
		LeaseEpoch: s.LeaseEpoch,
	}
	if s.CommitSHA != nil {
		view.CommitSHA = *s.CommitSHA
	}
	return view
}

func (s *Server) toolRenewLease(ctx context.Context, req Request) Response {
	if s.ports.LeaseRenewer == nil {
		return errResponse(req.ID, req.Tool, ErrCodeToolUnavailable, "lease renewer not configured")
	}
	expires, err := s.ports.LeaseRenewer.RenewLease(ctx, req.RunID, req.LeaseEpoch)
	if err != nil {
		return s.serviceError(req, err)
	}
	return okResponse(req.ID, req.Tool, map[string]any{"lease_expires_at": expires.UTC().Format("2006-01-02T15:04:05Z07:00")})
}

// serviceError maps a service failure to a stable error code. The message is
// returned to the extension (which surfaces it as evidence) but never logged
// with args, so a secret embedded in a command cannot reach the log.
func (s *Server) serviceError(req Request, err error) Response {
	s.log.Warn("spine tool failed",
		"tool", req.Tool,
		"run_id", req.RunID,
		"error", err.Error(),
	)
	return errResponse(req.ID, req.Tool, ErrCodeService, err.Error())
}
