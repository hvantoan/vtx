package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/execcmd"
	"github.com/vtx/platform/internal/gitops"
)

// VerificationStore fences every suite write against the run's current lease.
// FinishSuite atomically writes the result and its SHA-bound evidence artifact.
type VerificationStore interface {
	VerificationContext(context.Context, string) (RunSnapshot, string, error)
	NextVerificationAttempt(context.Context, string) (int32, error)
	BeginSuite(context.Context, VerifyCommand, VerificationRun) (VerificationRun, bool, error)
	FinishSuite(context.Context, VerifyCommand, VerificationRun, bool) error
	MarkSuperseded(context.Context, string, string) error
}
type VerificationRun struct {
	ID, RunID, SHA, Suite, Command, Status string
	Attempt                                int32
	Result                                 execcmd.Result
	ErrorDetail, LogRef                    string
	StartedAt, FinishedAt                  time.Time
}
type VerifyCommand struct {
	RunID, TaskID, LeaseOwner string
	LeaseEpoch                int64
	Attempt                   int32
}
type VerifyOutcome struct {
	AllPassed    bool
	SHA, Verdict string
	Failed       []string
	Retryable    bool
}
type LogSink interface {
	WriteLog(context.Context, VerificationRun) (string, error)
}
type HeadReader interface {
	HeadSHA(context.Context, string) (string, error)
}
type VerificationConfig struct{ SuiteTimeout, StageTimeout, LeaseCheckInterval time.Duration }
type VerificationService struct {
	Store  VerificationStore
	Runner execcmd.Runner
	Git    HeadReader
	Layout gitops.Layout
	Sink   LogSink
	Clock  Clock
	Config VerificationConfig
}

func CheckRunLease(run RunSnapshot, owner string, epoch int64, now time.Time) error {
	if run.State.IsTerminal() || run.LeaseOwner == nil || *run.LeaseOwner != owner || run.LeaseEpoch != epoch || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
		return domain.ErrStaleLease
	}
	return nil
}

// VerifyRun produces evidence only. The orchestrator applies transitions and
// bounded retry policy; functional failures never add a backward coding edge.
func (s *VerificationService) VerifyRun(ctx context.Context, cmd VerifyCommand) (VerifyOutcome, error) {
	clock := s.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	cfg := s.Config
	if cfg.SuiteTimeout <= 0 {
		cfg.SuiteTimeout = 10 * time.Minute
	}
	if cfg.StageTimeout <= 0 {
		cfg.StageTimeout = 30 * time.Minute
	}
	if cfg.LeaseCheckInterval <= 0 {
		cfg.LeaseCheckInterval = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.StageTimeout)
	defer cancel()
	run, path, err := s.Store.VerificationContext(ctx, cmd.RunID)
	if err != nil {
		return VerifyOutcome{}, err
	}
	if run.State != domain.StateVerifying {
		return VerifyOutcome{}, domain.ErrInvalidTransition
	}
	if err := CheckRunLease(run, cmd.LeaseOwner, cmd.LeaseEpoch, clock.Now()); err != nil {
		return VerifyOutcome{}, err
	}
	path, err = gitops.CanonicalPath(s.Layout.WorktreeRoot, path)
	if err != nil {
		return VerifyOutcome{}, err
	}
	sha, err := s.Git.HeadSHA(ctx, path)
	if err != nil {
		return VerifyOutcome{}, err
	}
	out := VerifyOutcome{SHA: sha, Verdict: "passed", AllPassed: true}
	if run.CommitSHA == nil || sha != *run.CommitSHA {
		if err := s.Store.MarkSuperseded(ctx, cmd.RunID, sha); err != nil {
			return out, err
		}
		out.AllPassed = false
		out.Verdict = "stale_sha"
		return out, nil
	}
	if cmd.Attempt == 0 {
		cmd.Attempt, err = s.Store.NextVerificationAttempt(ctx, cmd.RunID)
		if err != nil {
			return out, err
		}
	}
	// Monitor ownership while a subprocess is in flight, not only between suites.
	monitorCtx, stop := context.WithCancel(ctx)
	defer stop()
	monitorDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(cfg.LeaseCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				monitorDone <- nil
				return
			case <-ticker.C:
				current, _, e := s.Store.VerificationContext(monitorCtx, cmd.RunID)
				if e == nil {
					e = CheckRunLease(current, cmd.LeaseOwner, cmd.LeaseEpoch, clock.Now())
				}
				if e == nil && current.State != domain.StateVerifying {
					e = domain.ErrStaleLease
				}
				if e != nil {
					monitorDone <- e
					cancel()
					return
				}
			}
		}
	}()
	defer func() { stop(); <-monitorDone }()
	for _, spec := range execcmd.SuitePlan(path) {
		row := VerificationRun{RunID: cmd.RunID, SHA: sha, Attempt: cmd.Attempt, Suite: spec.Name, Command: execcmd.Command(spec), Status: "running", StartedAt: clock.Now()}
		row, fresh, err := s.Store.BeginSuite(ctx, cmd, row)
		if err != nil {
			return out, err
		}
		if !fresh {
			if row.Status == "running" {
				return out, fmt.Errorf("verification attempt already running: %w", domain.ErrEvidenceInvalid)
			}
		} else {
			if !out.AllPassed {
				row.Status = "skipped"
			} else {
				suiteCtx, suiteCancel := context.WithTimeout(ctx, cfg.SuiteTimeout)
				row.Result, err = s.Runner.Run(suiteCtx, spec)
				suiteCancel()
				row.Status = "passed"
				switch {
				case err != nil:
					row.Status = "error"
					row.ErrorDetail = "runner_error"
				case ctx.Err() != nil:
					row.Status = "error"
					row.ErrorDetail = "stage_cancelled"
				case row.Result.TimedOut:
					row.Status = "timeout"
				case row.Result.ExitCode != 0:
					row.Status = "failed"
				}
				head, headErr := s.Git.HeadSHA(ctx, path)
				if headErr != nil || head != sha {
					row.Status = "error"
					row.ErrorDetail = "sha_changed"
				}
			}
			row.FinishedAt = clock.Now()
			row.LogRef = "inline"
			if s.Sink != nil {
				if ref, e := s.Sink.WriteLog(ctx, row); e == nil {
					row.LogRef = ref
				}
			}
			// A cancelled runner still gets a bounded chance to persist non-green
			// evidence. Store fencing prevents a stale worker writing a green row.
			writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err = s.Store.FinishSuite(writeCtx, cmd, row, out.AllPassed && row.Status == "passed" && spec.Name == "make-build")
			writeCancel()
			if err != nil {
				return out, err
			}
		}
		if row.Status != "passed" && row.Status != "skipped" {
			out.AllPassed = false
			out.Verdict = row.Status
			out.Retryable = row.Status == "error" || row.Status == "timeout"
			out.Failed = append(out.Failed, spec.Name)
		}
	}
	return out, nil
}

var ErrVerificationFailed = errors.New("verification failed")
