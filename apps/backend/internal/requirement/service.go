package requirement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/integrations/jira"
)

type Input struct {
	Run                                   application.RunSnapshot
	SiteID, IssueID, IssueKey, ProjectKey string
	Worktree                              application.WorktreeView
}
type Baseline struct {
	VerificationID string   `json:"verification_run_id,omitempty"`
	Suite          string   `json:"suite"`
	Status         string   `json:"status"`
	ArtifactIDs    []string `json:"artifact_ids"`
}
type AgentDispatch struct {
	RunID        string   `json:"run_id"`
	Attempt      int32    `json:"attempt"`
	WorktreePath string   `json:"worktree_path"`
	Branch       string   `json:"branch"`
	BaseSHA      string   `json:"base_sha"`
	Requirement  Packet   `json:"requirement"`
	Baseline     Baseline `json:"baseline"`
}

// SuiteRunner is the VTX-8 execution boundary, injected so extraction never
// shells out itself. Adapters must execute argv without a shell and bound IO.
type SuiteRunner interface {
	RunBaseline(context.Context, application.WorktreeView, string) (SuiteResult, error)
}
type SuiteResult struct {
	Digest   string
	ExitCode int
	TimedOut bool
	Output   []byte
	Duration time.Duration
}
type Store interface {
	Input(context.Context, string) (Input, error)
	Requirement(context.Context, string) (Packet, bool, error)
	SaveRequirement(context.Context, Input, Packet, time.Time) (Packet, error)
	Baseline(context.Context, Input, string, time.Time, func(context.Context) (SuiteResult, error)) (Baseline, error)
	ReadBaseline(context.Context, Input) (Baseline, error)
	Block(context.Context, Input, string) error
	Renew(context.Context, Input, time.Time) error
}
type Config struct {
	Command           string
	Timeout, LeaseTTL time.Duration
}

func (c Config) defaults() Config {
	if c.Command == "" {
		c.Command = "make test"
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Minute
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 120 * time.Second
	}
	return c
}

type Service struct {
	store  Store
	jira   jira.IssueFetcher
	runner SuiteRunner
	cfg    Config
	clock  application.Clock
}

func New(store Store, fetcher jira.IssueFetcher, runner SuiteRunner, cfg Config, clock application.Clock) *Service {
	if clock == nil {
		clock = application.SystemClock{}
	}
	return &Service{store, fetcher, runner, cfg.defaults(), clock}
}
func (s *Service) Extract(ctx context.Context, runID string) (Packet, error) {
	in, err := s.store.Input(ctx, runID)
	if err != nil {
		return Packet{}, err
	}
	if in.Run.State != domain.StatePreparing {
		return Packet{}, domain.ErrInvalidTransition
	}
	if p, exists, err := s.store.Requirement(ctx, runID); err != nil {
		return Packet{}, err
	} else if exists {
		return p, nil
	}
	var issue jira.FetchedIssue
	for attempt := 0; attempt < 3; attempt++ {
		issue, err = s.jira.FetchIssue(ctx, in.IssueID)
		if err == nil {
			break
		}
		var upstream *jira.HTTPError
		if errors.As(err, &upstream) && upstream.Status >= 400 && upstream.Status < 500 {
			break
		}
		if ctx.Err() != nil {
			return Packet{}, ctx.Err()
		}
	}
	if err != nil {
		code := "jira_fetch_failed"
		var upstream *jira.HTTPError
		if errors.As(err, &upstream) {
			switch upstream.Status {
			case 401, 403:
				code = "jira_auth"
			case 404, 410:
				code = "jira_issue_gone"
			}
		}
		if blockErr := s.store.Block(ctx, in, code); blockErr != nil {
			return Packet{}, fmt.Errorf("block after Jira failure: %w", blockErr)
		}
		return Packet{}, fmt.Errorf("requirement extraction: %s", code)
	}
	if issue.ID != in.IssueID || issue.Key != in.IssueKey || issue.ProjectKey != in.ProjectKey {
		return Packet{}, fmt.Errorf("%w: Jira identity changed", application.ErrPayloadInvalid)
	}
	packet, err := Normalize(issue)
	if err != nil {
		return Packet{}, err
	}
	return s.store.SaveRequirement(ctx, in, packet, s.clock.Now())
}
func (s *Service) RecordBaseline(ctx context.Context, runID string) (Baseline, error) {
	in, err := s.store.Input(ctx, runID)
	if err != nil {
		return Baseline{}, err
	}
	if in.Run.State != domain.StatePreparing {
		return Baseline{}, domain.ErrInvalidTransition
	}
	if in.Worktree.Status != "allocated" || in.Worktree.BaseSHA == nil {
		if err := s.store.Block(ctx, in, "worktree_missing"); err != nil {
			return Baseline{}, err
		}
		return Baseline{}, application.ErrWorktreeNotFound
	}
	bounded, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	baseline, err := s.store.Baseline(bounded, in, s.cfg.Command, s.clock.Now(), func(runCtx context.Context) (SuiteResult, error) {
		if s.runner == nil {
			return SuiteResult{}, fmt.Errorf("baseline runner unavailable")
		}
		// Long baselines keep the admission lease alive; renewal failure cancels
		// execution and cannot produce dispatch-ready evidence.
		done := make(chan struct{})
		renewed := make(chan error, 1)
		go func() {
			ticker := time.NewTicker(s.cfg.LeaseTTL / 8)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					renewed <- nil
					return
				case <-runCtx.Done():
					renewed <- nil
					return
				case <-ticker.C:
					if err := s.store.Renew(runCtx, in, s.clock.Now().Add(s.cfg.LeaseTTL)); err != nil {
						cancel()
						renewed <- err
						return
					}
				}
			}
		}()
		result, err := s.runner.RunBaseline(runCtx, in.Worktree, s.cfg.Command)
		close(done)
		if renewErr := <-renewed; renewErr != nil {
			return result, fmt.Errorf("baseline lease renewal: %w", renewErr)
		}
		return result, err
	})
	if err != nil {
		return Baseline{}, err
	}
	if baseline.Status == "inconclusive" {
		if err := s.store.Block(ctx, in, "baseline_infra"); err != nil {
			return baseline, err
		}
		return baseline, fmt.Errorf("baseline infrastructure failure")
	}
	return baseline, nil
}
func (s *Service) Dispatch(ctx context.Context, runID string) (AgentDispatch, error) {
	in, err := s.store.Input(ctx, runID)
	if err != nil {
		return AgentDispatch{}, err
	}
	packet, exists, err := s.store.Requirement(ctx, runID)
	if err != nil {
		return AgentDispatch{}, err
	}
	if !exists {
		return AgentDispatch{}, domain.ErrEvidenceInvalid
	}
	baseline, err := s.store.ReadBaseline(ctx, in)
	if err != nil {
		return AgentDispatch{}, err
	}
	if in.Worktree.BaseSHA == nil || (baseline.Status != "passed" && baseline.Status != "failed") {
		return AgentDispatch{}, domain.ErrEvidenceInvalid
	}
	return AgentDispatch{RunID: runID, Attempt: in.Run.Attempt + 1, WorktreePath: in.Worktree.Path, Branch: in.Worktree.Branch, BaseSHA: *in.Worktree.BaseSHA, Requirement: packet, Baseline: baseline}, nil
}
