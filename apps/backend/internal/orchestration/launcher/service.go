package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/requirement"
)

// Store is the persistence surface the launcher needs. Everything except the
// claim ledger is the existing requirement store, so the launcher can never
// invent evidence: it reads what preparing committed and blocks when it is
// missing.
type Store interface {
	AllocatedRuns(context.Context) ([]application.AllocatedWorktreeView, error)
	Input(context.Context, string) (requirement.Input, error)
	Requirement(context.Context, string) (requirement.Packet, bool, error)
	ReadBaseline(context.Context, requirement.Input) (requirement.Baseline, error)
	Block(context.Context, requirement.Input, string) error
	Claim(context.Context, string, string, time.Time) (bool, error)
	Beat(context.Context, string, string) (bool, error)
	Release(context.Context, string, string) error
	// GrantLease writes a fresh lease epoch. Called only after the launcher
	// holds the claim and found no live lease of its own.
	GrantLease(context.Context, string, string, time.Time) (int64, error)
}

// Process is one spawned orchestrator. Wait blocks until it exits.
type Process interface {
	Wait() error
}

// Runner starts the agent process. Production is ExecRunner; tests inject a fake.
type Runner interface {
	Start(ctx context.Context, spec Spec, specPath string) (Process, error)
}

type Config struct {
	// Owner must equal the WorktreeService lease owner: the Pi extension renews
	// the run lease under this identity.
	Owner      string
	Tick       time.Duration
	LeaseRenew time.Duration
	// ClaimStale is how long a claim from a dead launcher is honoured before
	// another replica may take the run over.
	ClaimStale  time.Duration
	MaxParallel int
	SpecDir     string
	TestSuite   string
	Agents      []string
	Budget      Budget
	// LeaseTTL bounds a lease the launcher issues itself for a resumed run.
	LeaseTTL time.Duration
}

func (c Config) defaults() Config {
	if c.Owner == "" {
		c.Owner = application.DefaultLeaseOwner
	}
	if c.Tick <= 0 {
		c.Tick = 5 * time.Second
	}
	if c.LeaseRenew <= 0 {
		c.LeaseRenew = 30 * time.Second
	}
	if c.ClaimStale <= 0 {
		c.ClaimStale = 2 * time.Minute
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = 3
	}
	if c.SpecDir == "" {
		c.SpecDir = "./var/run/specs"
	}
	if c.TestSuite == "" {
		c.TestSuite = "make test"
	}
	if len(c.Agents) == 0 {
		c.Agents = append([]string(nil), DefaultAgents...)
	}
	if c.Budget.MaxTurns <= 0 {
		c.Budget.MaxTurns = 60
	}
	if c.Budget.MaxTokens <= 0 {
		c.Budget.MaxTokens = 200_000
	}
	if c.Budget.WallClockMS <= 0 {
		c.Budget.WallClockMS = (2 * time.Hour).Milliseconds()
	}
	if c.Budget.MaxDelegations <= 0 {
		c.Budget.MaxDelegations = 12
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 2 * time.Minute
	}
	return c
}

type Service struct {
	store  Store
	runner Runner
	cfg    Config
	clock  application.Clock
	log    *slog.Logger

	tickMu  sync.Mutex
	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

func New(store Store, runner Runner, cfg Config, clock application.Clock, log *slog.Logger) *Service {
	if clock == nil {
		clock = application.SystemClock{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, runner: runner, cfg: cfg.defaults(), clock: clock, log: log, running: map[string]bool{}}
}

// Run supervises until the context is cancelled. A child still running at
// shutdown is killed by its process-group cancel; its claim is released and the
// lease is left to expire, which the scheduler turns into a lease_lost block.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("orchestrator tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Wait blocks until every spawned process has been supervised.
func (s *Service) Wait() { s.wg.Wait() }

// Tick launches every eligible orchestrator-owned run up to MaxParallel. It is
// safe to call from a loop or a test; concurrent calls serialize.
func (s *Service) Tick(ctx context.Context) error {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()

	runs, err := s.store.AllocatedRuns(ctx)
	if err != nil {
		return fmt.Errorf("list allocated runs: %w", err)
	}
	now := s.clock.Now()
	for _, run := range runs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !s.eligible(run, now) || s.isRunning(run.RunID) {
			continue
		}
		if s.activeCount() >= s.cfg.MaxParallel {
			s.log.Info("orchestrator capacity full", "max_parallel", s.cfg.MaxParallel)
			break
		}
		if err := s.launch(ctx, run.RunID); err != nil {
			s.log.Error("orchestrator launch failed", "run_id", run.RunID, "error", err)
		}
	}
	return nil
}

func (s *Service) eligible(run application.AllocatedWorktreeView, now time.Time) bool {
	if !drivable(domain.State(run.RunState)) {
		return false
	}
	if !leaseLive(run.RunLeaseOwner, run.RunLeaseExpireAt, "", now) {
		// No live lease: an operator resume returned the run to an orchestrator
		// stage after the block released it; launch() grants a fresh epoch.
		return true
	}
	// A live lease is another driver's fence: never steal it. Our own live lease
	// is a relaunch after a launcher crash within the TTL, and is reused as-is.
	return run.RunLeaseOwner != nil && *run.RunLeaseOwner == s.cfg.Owner
}

// leaseLive reports whether a lease is unexpired and, when owner is non-empty,
// held by that owner.
func leaseLive(owner *string, expiresAt *time.Time, want string, now time.Time) bool {
	if expiresAt == nil || !expiresAt.After(now) {
		return false
	}
	if want == "" {
		return true
	}
	return owner != nil && *owner == want
}

func (s *Service) isRunning(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[runID]
}

func (s *Service) activeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

func (s *Service) markRunning(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[runID] = true
}

func (s *Service) unmarkRunning(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, runID)
}

// drivable reports whether an orchestrator session owns this stage. A relaunch
// in a later stage continues the pipeline after a crash instead of orphaning
// the run; queued/preparing belong to other workers and blocked/retry_wait
// wait for policy or an operator.
func drivable(state domain.State) bool {
	switch state {
	case domain.StateCoding, domain.StateVerifying, domain.StateQA, domain.StateQualityGate, domain.StatePRDraft:
		return true
	}
	return false
}

func (s *Service) launch(ctx context.Context, runID string) error {
	in, err := s.store.Input(ctx, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if !drivable(in.Run.State) {
		return nil
	}
	packet, ok, err := s.store.Requirement(ctx, runID)
	if err != nil {
		return fmt.Errorf("load requirement: %w", err)
	}
	if !ok {
		return s.block(ctx, runID, "requirement_missing")
	}
	baseline, err := s.store.ReadBaseline(ctx, in)
	if err != nil {
		return s.block(ctx, runID, "baseline_missing")
	}
	token, err := newClaimToken()
	if err != nil {
		return err
	}
	claimed, err := s.store.Claim(ctx, runID, token, s.clock.Now().Add(-s.cfg.ClaimStale))
	if err != nil {
		return fmt.Errorf("claim run: %w", err)
	}
	if !claimed {
		return nil
	}

	// Take the fence only after winning the claim, so concurrent launchers
	// cannot bump each other's epoch or spawn two children for one run.
	now := s.clock.Now()
	if !leaseLive(in.Run.LeaseOwner, in.Run.LeaseExpiresAt, s.cfg.Owner, now) {
		expires := now.Add(s.cfg.LeaseTTL)
		epoch, err := s.store.GrantLease(ctx, runID, s.cfg.Owner, expires)
		if err != nil {
			s.releaseClaim(runID, token)
			return fmt.Errorf("grant run lease: %w", err)
		}
		owner := s.cfg.Owner
		in.Run.LeaseOwner = &owner
		in.Run.LeaseEpoch = epoch
		in.Run.LeaseExpiresAt = &expires
	}

	spec, err := BuildSpec(in, packet, baseline, s.cfg)
	if err != nil {
		s.releaseClaim(runID, token)
		return s.block(ctx, runID, "orchestrator_spec_invalid")
	}

	specPath, err := WriteSpec(s.cfg.SpecDir, spec)
	if err != nil {
		s.releaseClaim(runID, token)
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	proc, err := s.runner.Start(runCtx, spec, specPath)
	if err != nil {
		cancel()
		s.releaseClaim(runID, token)
		return s.block(ctx, runID, "orchestrator_spawn_failed")
	}

	s.markRunning(runID)
	s.wg.Add(1)
	s.log.Info("orchestrator started",
		"run_id", runID,
		"lease_owner", spec.Lease.Owner,
		"lease_epoch", spec.Lease.Epoch,
		"spec", specPath,
	)
	go s.supervise(runCtx, cancel, runID, token, proc)
	return nil
}

// supervise owns exactly one child. It keeps the claim warm, reacts to claim
// loss by killing the child, and records a block when the child exits while
// the run is still mid-pipeline.
func (s *Service) supervise(ctx context.Context, cancel context.CancelFunc, runID, token string, proc Process) {
	defer s.wg.Done()
	defer s.unmarkRunning(runID)
	defer cancel()
	defer s.releaseClaim(runID, token)

	beatDone := make(chan struct{})
	if s.cfg.LeaseRenew > 0 {
		go func() {
			ticker := time.NewTicker(s.cfg.LeaseRenew)
			defer ticker.Stop()
			for {
				select {
				case <-beatDone:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					beatCtx, beatCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					held, err := s.store.Beat(beatCtx, runID, token)
					beatCancel()
					if err != nil {
						s.log.Error("orchestrator claim heartbeat failed", "run_id", runID, "error", err)
						continue
					}
					if !held {
						s.log.Warn("orchestrator claim lost; stopping the child", "run_id", runID)
						cancel()
						return
					}
				}
			}
		}()
	}

	err := proc.Wait()
	close(beatDone)
	if ctx.Err() != nil {
		s.log.Info("orchestrator stopped", "run_id", runID)
		return
	}
	if err != nil {
		s.log.Warn("orchestrator exited with error", "run_id", runID, "error", err)
		s.blockDetached(runID, "orchestrator_failed")
		return
	}
	in, loadErr := s.store.Input(context.WithoutCancel(ctx), runID)
	if loadErr != nil {
		s.log.Error("orchestrator exit: run unreadable", "run_id", runID, "error", loadErr)
		return
	}
	// Leaving the pipeline mid-stage strands a non-terminal run with no driver.
	// pr_draft is the hand-off point (the PR is opened; merging is the
	// operator's), so only parked/terminal states count as finished.
	switch in.Run.State {
	case domain.StatePRDraft, domain.StateBlocked, domain.StateRetryWait,
		domain.StateCompleted, domain.StateFailed, domain.StateCancelled:
		s.log.Info("orchestrator finished", "run_id", runID, "state", in.Run.State)
	default:
		s.log.Warn("orchestrator exited before the pipeline finished", "run_id", runID, "state", in.Run.State)
		s.blockDetached(runID, "orchestrator_stalled")
	}
}

func (s *Service) block(ctx context.Context, runID, code string) error {
	in, err := s.store.Input(ctx, runID)
	if err != nil {
		return fmt.Errorf("block %s: %w", code, err)
	}
	return s.store.Block(ctx, in, code)
}

// blockDetached runs after the child exited: the parent context is already
// cancelled on shutdown, so bookkeeping must outlive it.
func (s *Service) blockDetached(runID, code string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.block(ctx, runID, code); err != nil && !errors.Is(err, domain.ErrStaleLease) {
		s.log.Error("orchestrator block failed", "run_id", runID, "code", code, "error", err)
	}
}

func (s *Service) releaseClaim(runID, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.Release(ctx, runID, token); err != nil {
		s.log.Error("orchestrator claim release failed", "run_id", runID, "error", err)
	}
}

// WriteSpec persists the spec with owner-only permissions and atomic rename so
// a reader never sees a partial file.
func WriteSpec(dir string, spec Spec) (string, error) {
	if !validRunID(spec.RunID) {
		return "", fmt.Errorf("invalid run id %q", spec.RunID)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create spec dir: %w", err)
	}
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode spec: %w", err)
	}
	path := filepath.Join(dir, spec.RunID+".json")
	tmp, err := os.CreateTemp(dir, spec.RunID+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create spec: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write spec: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("publish spec: %w", err)
	}
	return path, nil
}

// validRunID keeps a database-sourced identifier from ever escaping SpecDir.
func validRunID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch {
		case r == '-':
			if i != 8 && i != 13 && i != 18 && i != 23 {
				return false
			}
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
		default:
			return false
		}
	}
	return true
}
