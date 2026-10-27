package launcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/requirement"
)

type fakeStore struct {
	mu        sync.Mutex
	runs      []application.AllocatedWorktreeView
	inputs    map[string]requirement.Input
	packets   map[string]requirement.Packet
	baselines map[string]requirement.Baseline
	blocks    []string
	claimOK   bool
	claims    int
	beats     int
	beatOK    bool
	releases  int
	grants    int
}

func newFakeStore(in requirement.Input) *fakeStore {
	return &fakeStore{
		runs: []application.AllocatedWorktreeView{{
			WorktreeView:     in.Worktree,
			RunState:         string(in.Run.State),
			RunLeaseOwner:    in.Run.LeaseOwner,
			RunLeaseEpoch:    in.Run.LeaseEpoch,
			RunLeaseExpireAt: in.Run.LeaseExpiresAt,
		}},
		inputs:    map[string]requirement.Input{in.Run.RunID: in},
		packets:   map[string]requirement.Packet{in.Run.RunID: testPacket()},
		baselines: map[string]requirement.Baseline{in.Run.RunID: testBaseline()},
		claimOK:   true,
		beatOK:    true,
	}
}

func (f *fakeStore) AllocatedRuns(context.Context) ([]application.AllocatedWorktreeView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]application.AllocatedWorktreeView(nil), f.runs...), nil
}

func (f *fakeStore) Input(_ context.Context, runID string) (requirement.Input, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.inputs[runID]
	if !ok {
		return requirement.Input{}, errors.New("missing run")
	}
	return in, nil
}

func (f *fakeStore) Requirement(_ context.Context, runID string) (requirement.Packet, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.packets[runID]
	return p, ok, nil
}

func (f *fakeStore) ReadBaseline(_ context.Context, in requirement.Input) (requirement.Baseline, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.baselines[in.Run.RunID]
	if !ok {
		return requirement.Baseline{}, errors.New("missing baseline")
	}
	return b, nil
}

func (f *fakeStore) Block(_ context.Context, in requirement.Input, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocks = append(f.blocks, code)
	return nil
}

func (f *fakeStore) Claim(context.Context, string, string, time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.claimOK {
		return false, nil
	}
	f.claims++
	return true, nil
}

func (f *fakeStore) Beat(context.Context, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats++
	return f.beatOK, nil
}

func (f *fakeStore) Release(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	return nil
}

func (f *fakeStore) GrantLease(_ context.Context, runID, owner string, expiresAt time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.inputs[runID]
	if !ok {
		return 0, errors.New("missing run")
	}
	f.grants++
	in.Run.LeaseOwner = &owner
	in.Run.LeaseEpoch++
	in.Run.LeaseExpiresAt = &expiresAt
	f.inputs[runID] = in
	return in.Run.LeaseEpoch, nil
}

func (f *fakeStore) setState(runID string, state domain.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.inputs[runID]
	in.Run.State = state
	f.inputs[runID] = in
}

func (f *fakeStore) blocksSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.blocks...)
}

type fakeProcess struct {
	err  error
	done chan struct{}
}

func (p *fakeProcess) Wait() error {
	if p.done != nil {
		<-p.done
	}
	return p.err
}

type fakeRunner struct {
	mu      sync.Mutex
	started []Spec
	paths   []string
	proc    Process
	err     error
	onStart func(spec Spec)
}

func (r *fakeRunner) Start(_ context.Context, spec Spec, specPath string) (Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	r.started = append(r.started, spec)
	r.paths = append(r.paths, specPath)
	if r.onStart != nil {
		r.onStart(spec)
	}
	if r.proc == nil {
		return &fakeProcess{}, nil
	}
	return r.proc, nil
}

func (r *fakeRunner) specs() []Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Spec(nil), r.started...)
}

func newTestService(t *testing.T, store Store, runner Runner, cfg Config) *Service {
	t.Helper()
	cfg.SpecDir = t.TempDir()
	if cfg.Owner == "" {
		cfg.Owner = "worktree-service"
	}
	if cfg.LeaseRenew == 0 {
		cfg.LeaseRenew = time.Hour // effectively no heartbeat during a unit test
	}
	return New(store, runner, cfg, application.SystemClock{}, nil)
}

func TestTickLaunchesEligibleRunAndReleasesClaim(t *testing.T) {
	now := time.Now()
	in := testInput(now, domain.StateCoding)
	store := newFakeStore(in)
	runner := &fakeRunner{onStart: func(spec Spec) { store.setState(spec.RunID, domain.StatePRDraft) }}
	svc := newTestService(t, store, runner, Config{})

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	svc.Wait()

	specs := runner.specs()
	if len(specs) != 1 {
		t.Fatalf("expected 1 launch, got %d", len(specs))
	}
	if specs[0].RunID != testRunID || specs[0].Lease.Epoch != 3 {
		t.Fatalf("spec wrong: %+v", specs[0])
	}
	store.mu.Lock()
	claims, releases := store.claims, store.releases
	store.mu.Unlock()
	if claims != 1 || releases != 1 {
		t.Fatalf("claim/release = %d/%d, want 1/1", claims, releases)
	}
	if blocks := store.blocksSeen(); len(blocks) != 0 {
		t.Fatalf("unexpected blocks: %v", blocks)
	}
}

func TestTickSkipsIneligibleRuns(t *testing.T) {
	now := time.Now()
	cases := map[string]func(*fakeStore, *requirement.Input){
		"queued":      func(s *fakeStore, _ *requirement.Input) { s.runs[0].RunState = string(domain.StateQueued) },
		"blocked":     func(s *fakeStore, _ *requirement.Input) { s.runs[0].RunState = string(domain.StateBlocked) },
		"other owner": func(s *fakeStore, _ *requirement.Input) { other := "someone-else"; s.runs[0].RunLeaseOwner = &other },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := testInput(now, domain.StateCoding)
			store := newFakeStore(in)
			mutate(store, &in)
			runner := &fakeRunner{}
			svc := newTestService(t, store, runner, Config{})
			if err := svc.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			svc.Wait()
			if got := len(runner.specs()); got != 0 {
				t.Fatalf("expected no launch, got %d", got)
			}
		})
	}
}

// An operator resume parks a run back in coding after the block released the
// lease. Nothing else re-grants one, so the launcher takes a fresh epoch itself.
func TestTickRegrantsLeaseForResumedRun(t *testing.T) {
	now := time.Now()
	cases := map[string]func(*requirement.Input){
		"no lease": func(in *requirement.Input) {
			in.Run.LeaseOwner = nil
			in.Run.LeaseExpiresAt = nil
		},
		"expired foreign lease": func(in *requirement.Input) {
			owner := "someone-else"
			past := now.Add(-time.Minute)
			in.Run.LeaseOwner = &owner
			in.Run.LeaseExpiresAt = &past
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := testInput(now, domain.StateCoding)
			mutate(&in)
			store := newFakeStore(in)
			runner := &fakeRunner{onStart: func(spec Spec) { store.setState(spec.RunID, domain.StateVerifying) }}
			svc := newTestService(t, store, runner, Config{Owner: "worktree-service", LeaseTTL: time.Minute})

			if err := svc.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			svc.Wait()

			specs := runner.specs()
			if len(specs) != 1 {
				t.Fatalf("expected 1 launch, got %d", len(specs))
			}
			if specs[0].Lease.Owner != "worktree-service" || specs[0].Lease.Epoch != in.Run.LeaseEpoch+1 {
				t.Fatalf("spec lease = %+v, want owner worktree-service epoch %d", specs[0].Lease, in.Run.LeaseEpoch+1)
			}
			store.mu.Lock()
			grants := store.grants
			store.mu.Unlock()
			if grants != 1 {
				t.Fatalf("grant calls = %d, want 1", grants)
			}
		})
	}
}

func TestTickHonoursLostClaim(t *testing.T) {
	store := newFakeStore(testInput(time.Now(), domain.StateCoding))
	store.claimOK = false
	runner := &fakeRunner{}
	svc := newTestService(t, store, runner, Config{})
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	svc.Wait()
	if got := len(runner.specs()); got != 0 {
		t.Fatalf("expected no launch when the claim is held elsewhere, got %d", got)
	}
}

func TestNonZeroExitBlocksRun(t *testing.T) {
	store := newFakeStore(testInput(time.Now(), domain.StateCoding))
	runner := &fakeRunner{proc: &fakeProcess{err: errors.New("exit 1")}}
	svc := newTestService(t, store, runner, Config{})
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	svc.Wait()
	assertBlocked(t, store, "orchestrator_failed")
}

func TestZeroExitMidPipelineBlocksRun(t *testing.T) {
	for _, state := range []domain.State{domain.StateCoding, domain.StateVerifying, domain.StateQA, domain.StateQualityGate} {
		t.Run(string(state), func(t *testing.T) {
			store := newFakeStore(testInput(time.Now(), state))
			runner := &fakeRunner{}
			svc := newTestService(t, store, runner, Config{})
			if err := svc.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			svc.Wait()
			assertBlocked(t, store, "orchestrator_stalled")
		})
	}
}

func TestZeroExitInPRDraftIsFinished(t *testing.T) {
	store := newFakeStore(testInput(time.Now(), domain.StatePRDraft))
	runner := &fakeRunner{}
	svc := newTestService(t, store, runner, Config{})
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	svc.Wait()
	if blocks := store.blocksSeen(); len(blocks) != 0 {
		t.Fatalf("pr_draft is the hand-off point, got blocks %v", blocks)
	}
	if got := len(runner.specs()); got != 1 {
		t.Fatalf("expected a relaunch in pr_draft, got %d", got)
	}
}

func TestMissingRequirementBlocksRun(t *testing.T) {
	store := newFakeStore(testInput(time.Now(), domain.StateCoding))
	delete(store.packets, testRunID)
	runner := &fakeRunner{}
	svc := newTestService(t, store, runner, Config{})
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	svc.Wait()
	if got := len(runner.specs()); got != 0 {
		t.Fatalf("expected no launch, got %d", got)
	}
	assertBlocked(t, store, "requirement_missing")
}

func TestSpawnFailureBlocksRunAndReleasesClaim(t *testing.T) {
	store := newFakeStore(testInput(time.Now(), domain.StateCoding))
	runner := &fakeRunner{err: errors.New("no binary")}
	svc := newTestService(t, store, runner, Config{})
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	assertBlocked(t, store, "orchestrator_spawn_failed")
	store.mu.Lock()
	releases := store.releases
	store.mu.Unlock()
	if releases != 1 {
		t.Fatalf("claim not released after spawn failure: %d", releases)
	}
}

func TestCapacityLimit(t *testing.T) {
	now := time.Now()
	first := testInput(now, domain.StateCoding)
	second := testInput(now, domain.StateCoding)
	second.Run.RunID = "99999999-8888-4777-8666-555555555555"
	second.Worktree.RunID = second.Run.RunID
	store := newFakeStore(first)
	store.runs = append(store.runs, application.AllocatedWorktreeView{
		WorktreeView:     second.Worktree,
		RunState:         string(second.Run.State),
		RunLeaseOwner:    second.Run.LeaseOwner,
		RunLeaseEpoch:    second.Run.LeaseEpoch,
		RunLeaseExpireAt: second.Run.LeaseExpiresAt,
	})
	store.inputs[second.Run.RunID] = second
	store.packets[second.Run.RunID] = testPacket()
	store.baselines[second.Run.RunID] = testBaseline()

	blocked := make(chan struct{})
	runner := &fakeRunner{proc: &fakeProcess{done: blocked}}
	svc := newTestService(t, store, runner, Config{MaxParallel: 1, LeaseRenew: time.Hour})
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := len(runner.specs()); got != 1 {
		t.Fatalf("expected capacity-limited launch, got %d", got)
	}
	close(blocked)
	svc.Wait()
}

func assertBlocked(t *testing.T, store *fakeStore, want string) {
	t.Helper()
	blocks := store.blocksSeen()
	if len(blocks) != 1 || blocks[0] != want {
		t.Fatalf("blocks = %v, want [%s]", blocks, want)
	}
}
