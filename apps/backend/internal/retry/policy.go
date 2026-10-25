// Package retry supplies bounded, versioned stage-failure policy. It never
// introduces backward stage edges or revives terminal runs.
package retry

import (
	"context"
	"errors"
	"fmt"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"math"
	"math/rand/v2"
	"time"
)

type FailureClass string

const (
	ClassAgentCrashed     FailureClass = "agent_crashed"
	ClassLeaseLost        FailureClass = "lease_lost"
	ClassWorktreeLost     FailureClass = "worktree_lost"
	ClassInfraTransient   FailureClass = "infra_transient"
	ClassEvidencePending  FailureClass = "evidence_pending"
	ClassVerificationFail FailureClass = "verification_failed"
	ClassQAVerdictFail    FailureClass = "qa_verdict_fail"
	ClassGateFail         FailureClass = "gate_fail"
	ClassNonRetryable     FailureClass = "non_retryable"
	ClassOperatorBlocked  FailureClass = "operator_blocked"
)

var ErrAgentCrashed = errors.New("agent process crashed")

func Classify(err error) FailureClass {
	switch {
	case errors.Is(err, ErrAgentCrashed), errors.Is(err, context.DeadlineExceeded):
		return ClassAgentCrashed
	case errors.Is(err, domain.ErrStaleLease), errors.Is(err, application.ErrWorktreeLeaseLost):
		return ClassLeaseLost
	case errors.Is(err, application.ErrWorktreeNotFound):
		return ClassWorktreeLost
	case errors.Is(err, application.ErrPayloadInvalid):
		return ClassNonRetryable
	case errors.Is(err, application.ErrVerificationFailed):
		return ClassVerificationFail
	case errors.Is(err, domain.ErrEvidenceInvalid):
		return ClassEvidencePending
	default:
		return ClassInfraTransient
	}
}

type Disposition string

const (
	Retry Disposition = "retry"
	Fail  Disposition = "fail"
	Block Disposition = "block"
)

type Policy struct {
	Version                   string
	MaxAttempts               int32
	BaseDelay, MaxDelay       time.Duration
	BackoffFactor, JitterFrac float64
	ClassAction               map[FailureClass]Disposition
	Random                    func() float64
}

func DefaultPolicy() Policy {
	return Policy{Version: "v1", MaxAttempts: 3, BaseDelay: 30 * time.Second, MaxDelay: 10 * time.Minute, BackoffFactor: 2, JitterFrac: .2, Random: rand.Float64, ClassAction: map[FailureClass]Disposition{ClassAgentCrashed: Retry, ClassLeaseLost: Retry, ClassWorktreeLost: Retry, ClassInfraTransient: Retry, ClassEvidencePending: Retry, ClassVerificationFail: Fail, ClassQAVerdictFail: Fail, ClassGateFail: Fail, ClassNonRetryable: Block, ClassOperatorBlocked: Block}}
}
func (p Policy) Validate() error {
	if p.Version == "" || p.MaxAttempts < 1 || p.BaseDelay <= 0 || p.MaxDelay < p.BaseDelay || p.BackoffFactor < 1 || math.IsNaN(p.BackoffFactor) || math.IsInf(p.BackoffFactor, 0) || p.JitterFrac < 0 || p.JitterFrac > 1 || math.IsNaN(p.JitterFrac) {
		return fmt.Errorf("invalid retry policy")
	}
	return nil
}
func (p Policy) Backoff(attempt int32) time.Duration {
	delay := float64(p.BaseDelay) * math.Pow(p.BackoffFactor, float64(attempt-1))
	if delay > float64(p.MaxDelay) || math.IsInf(delay, 0) {
		delay = float64(p.MaxDelay)
	}
	sample := .5
	if p.Random != nil {
		sample = p.Random()
	}
	sample = math.Max(0, math.Min(1, sample))
	return time.Duration(delay * (1 + p.JitterFrac*(2*sample-1)))
}

// PlanFailure runs on the locked run inside RegistryService's transaction.
// The version snapshot is immutable; a different policy cannot silently take
// over a run whose first retry used another version.
func (p Policy) PlanFailure(run application.RunSnapshot, cmd application.TransitionCommand, now time.Time) (application.TransitionCommand, error) {
	if err := p.Validate(); err != nil {
		return cmd, err
	}
	if run.State.IsTerminal() {
		return cmd, domain.ErrAlreadyTerminal
	}
	if run.RetryPolicyVersion != "" && run.RetryPolicyVersion != p.Version {
		return cmd, fmt.Errorf("retry policy version unavailable: %s", run.RetryPolicyVersion)
	}
	cls := FailureClass(cmd.FailureClass)
	if cls == "" {
		cls = ClassInfraTransient
	}
	disposition, ok := p.ClassAction[cls]
	if !ok {
		cls = ClassNonRetryable
		disposition = Block
	}
	cmd.FailureClass = string(cls)
	cmd.RetryPolicyVersion = p.Version
	cmd.ErrorCode = string(cls)
	switch disposition {
	case Retry:
		if run.Attempt >= p.MaxAttempts {
			cmd.Action = domain.ActionFail
			cmd.ErrorCode = "retry_budget_exhausted"
			cmd.RetryDueAt = nil
		} else {
			cmd.Action = domain.ActionScheduleRetry
			due := now.Add(p.Backoff(run.Attempt + 1))
			cmd.RetryDueAt = &due
		}
	case Fail:
		cmd.Action = domain.ActionFail
	case Block:
		cmd.Action = domain.ActionBlock
	default:
		return cmd, fmt.Errorf("invalid retry disposition %q", disposition)
	}
	metadata, err := application.MergeRetryMetadata(cmd.Metadata, map[string]any{"failure_class": cmd.FailureClass, "retry_policy_version": p.Version, "attempts_used": run.Attempt, "attempts_max": p.MaxAttempts, "resume_state": run.State, "error_code": cmd.ErrorCode})
	if err != nil {
		return cmd, err
	}
	cmd.Metadata = metadata
	if cmd.RetryDueAt != nil {
		metadata, err = application.MergeRetryMetadata(cmd.Metadata, map[string]any{"backoff_ms": cmd.RetryDueAt.Sub(now).Milliseconds(), "next_due_at": cmd.RetryDueAt.Format(time.RFC3339Nano)})
		if err != nil {
			return cmd, err
		}
		cmd.Metadata = metadata
	}
	return cmd, nil
}
