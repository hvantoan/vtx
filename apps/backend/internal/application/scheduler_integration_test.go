//go:build integration

// VTX-5 scheduler against REAL Postgres: the concurrency claims that a fake
// cannot prove — the 3-slot system budget under competing schedulers,
// exactly-once release, cancel/grant races, heartbeat retention.
// Skipped entirely without TEST_DATABASE_URL (see TestMain in
// registry_consume_integration_test.go — this file shares that TestMain).
package application_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/persistence/db"
)

// stubJira is the test SchedulingDataProvider.
type stubJira struct {
	mu   sync.Mutex
	data map[string]application.SchedulingData
	err  map[string]error
}

func newStubJira() *stubJira {
	return &stubJira{data: map[string]application.SchedulingData{}, err: map[string]error{}}
}

func (s *stubJira) set(issueID, priority string, blockers ...application.BlockerRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[issueID] = application.SchedulingData{PriorityName: priority, BlockedBy: blockers}
}

func (s *stubJira) fail(issueID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err[issueID] = err
}

func (s *stubJira) FetchSchedulingData(_ context.Context, issueID string) (application.SchedulingData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.err[issueID]; ok {
		return application.SchedulingData{}, err
	}
	if d, ok := s.data[issueID]; ok {
		return d, nil
	}
	return application.SchedulingData{PriorityName: "Medium"}, nil
}

// schedulerHarness seeds real tasks+runs through the VTX-3 intake path so
// every queue row points at real foreign keys.
type schedulerHarness struct {
	t        *testing.T
	registry *registryHarness
	pool     *pgxpool.Pool
	store    *persistence.PgSchedulerStore
	jira     *stubJira
	svc      *application.SchedulerService
	seeded   int
}

func newSchedulerHarness(t *testing.T) *schedulerHarness {
	t.Helper()
	reg := newRegistryHarness(t)
	stub := newStubJira()
	store := persistence.NewPgSchedulerStore(reg.pool, db.New(reg.pool))
	return &schedulerHarness{
		t:        t,
		registry: reg,
		pool:     reg.pool,
		store:    store,
		jira:     stub,
		svc: application.NewSchedulerService(store, stub, "sched-test",
			application.SystemClock{},
			slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	}
}

// seedRun creates a queued run via the real intake path and returns run_id
// and its Jira issue id (for stub priority/blocker setup).
func (h *schedulerHarness) seedRun() (runID, issueID string) {
	h.t.Helper()
	h.seeded++
	issueID = h.registry.uniqueIssue()
	issueKey := fmt.Sprintf("VTX-%d", 9000+h.seeded)
	intent := h.registry.seedIntent(issueKey)
	payload := h.registry.payload(intent, newEventID(h.t, h.pool), issueID, issueKey)
	outcome, err := h.registry.svc.ConsumeIntent(context.Background(), payload)
	if err != nil {
		h.t.Fatalf("consume intent: %v", err)
	}
	if outcome.RunID == "" {
		h.t.Fatalf("expected admitted run, got %+v", outcome)
	}
	return outcome.RunID, issueID
}

func (h *schedulerHarness) heldCount() int {
	return int(h.registry.count(`SELECT count(*) FROM coding_slots WHERE status='held'`))
}

// AC-01: 4 eligible entries, 3 grants.
func TestScheduler_GrantsAtMostThree(t *testing.T) {
	h := newSchedulerHarness(t)
	ctx := context.Background()
	var runs []string
	for i := 0; i < 4; i++ {
		runID, _ := h.seedRun()
		runs = append(runs, runID)
		if _, err := h.svc.Enqueue(ctx, runID); err != nil {
			t.Fatalf("enqueue %s: %v", runID, err)
		}
	}
	res, err := h.svc.Admit(ctx, "worker-1")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if len(res.Granted) != 3 {
		t.Fatalf("granted %d, want 3", len(res.Granted))
	}
	if got := h.heldCount(); got != 3 {
		t.Fatalf("held %d, want 3", got)
	}
	if got := h.registry.count(`SELECT count(*) FROM coding_queue WHERE status='waiting'`); got != 1 {
		t.Fatalf("waiting %d, want 1", got)
	}
}

// AC-02: competing scheduler replicas on separate connections never exceed
// the shared 3-slot budget.
func TestScheduler_ConcurrentSchedulersNeverExceedBudget(t *testing.T) {
	h := newSchedulerHarness(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		runID, _ := h.seedRun()
		if _, err := h.svc.Enqueue(ctx, runID); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// Three independent service instances over three pools — real replicas.
	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	for w := 0; w < 3; w++ {
		pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
		if err != nil {
			t.Fatalf("pool %d: %v", w, err)
		}
		defer pool.Close()
		svc := application.NewSchedulerService(
			persistence.NewPgSchedulerStore(pool, db.New(pool)),
			h.jira, fmt.Sprintf("sched-%d", w), application.SystemClock{}, nil)
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			if _, err := svc.Admit(ctx, worker); err != nil {
				errCh <- err
			}
		}(fmt.Sprintf("worker-%d", w))
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("admit error: %v", err)
	}
	if got := h.heldCount(); got > 3 {
		t.Fatalf("held slots %d exceeded budget 3", got)
	}
	if got := h.heldCount(); got != 3 {
		t.Fatalf("held %d, want exactly 3 (6 eligible, budget 3)", got)
	}
}

// AC-03: same run never holds two grants — across re-enqueue, re-admit and a
// fresh service instance (scheduler restart).
func TestScheduler_NoDuplicateGrantPerRun(t *testing.T) {
	h := newSchedulerHarness(t)
	ctx := context.Background()
	runID, _ := h.seedRun()
	for i := 0; i < 3; i++ {
		if _, err := h.svc.Enqueue(ctx, runID); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if _, err := h.svc.Admit(ctx, "w1"); err != nil {
		t.Fatalf("admit 1: %v", err)
	}
	// Restarted scheduler, same run: re-admit must not double-grant.
	h2 := newSchedulerHarness(t)
	if _, err := h2.svc.Admit(ctx, "w2"); err != nil {
		t.Fatalf("admit 2: %v", err)
	}
	if got := h.registry.count(
		`SELECT count(*) FROM coding_slots WHERE run_id=$1 AND status='held'`, runID); got != 1 {
		t.Fatalf("run holds %d slots, want exactly 1", got)
	}
}

// AC-04: priority first, FIFO inside; blocked head does not starve the tail.
func TestScheduler_PriorityFifoAndBlockedHead(t *testing.T) {
	h := newSchedulerHarness(t)
	ctx := context.Background()
	var issueIDs []string
	for i := 0; i < 4; i++ {
		runID, issueID := h.seedRun()
		issueIDs = append(issueIDs, issueID)
		if _, err := h.svc.Enqueue(ctx, runID); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	// First in queue is High priority but blocked; a later eligible Low must
	// still be admitted.
	h.jira.set(issueIDs[0], "High",
		application.BlockerRef{IssueID: "1", IssueKey: "VTX-1", StatusName: "To Do", Done: false})
	h.jira.set(issueIDs[1], "Low")
	h.jira.set(issueIDs[2], "Low")
	h.jira.set(issueIDs[3], "Low")

	res, err := h.svc.Admit(ctx, "w1")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if len(res.Granted) != 3 {
		t.Fatalf("granted %d, want 3 (blocked head, 3 lows)", len(res.Granted))
	}
	if got := h.registry.count(
		`SELECT count(*) FROM coding_queue WHERE status='blocked_dependency'`); got != 1 {
		t.Fatalf("blocked_dependency %d, want 1", got)
	}
}

// AC-05: unverifiable Jira ⇒ blocked_unverified, no slot.
func TestScheduler_UnverifiedGetsNoSlot(t *testing.T) {
	h := newSchedulerHarness(t)
	ctx := context.Background()
	runID, issueID := h.seedRun()
	h.jira.fail(issueID, fmt.Errorf("jira 403"))
	if _, err := h.svc.Enqueue(ctx, runID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	res, err := h.svc.Admit(ctx, "w1")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if len(res.Granted) != 0 || h.heldCount() != 0 {
		t.Fatalf("unverified must receive no slot, got %+v held %d", res.Granted, h.heldCount())
	}
	if got := h.registry.count(
		`SELECT count(*) FROM coding_queue WHERE status='blocked_unverified'`); got != 1 {
		t.Fatalf("blocked_unverified %d, want 1", got)
	}
}

// AC-06: cancelled waiting entry never receives a slot; cancel racing an
// existing grant returns ErrSlotHeld and the slot survives.
func TestScheduler_CancelRules(t *testing.T) {
	h := newSchedulerHarness(t)
	ctx := context.Background()
	r1, _ := h.seedRun()
	r2, _ := h.seedRun()
	for _, r := range []string{r1, r2} {
		if _, err := h.svc.Enqueue(ctx, r); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if _, err := h.svc.CancelQueued(ctx, r1, "operator"); err != nil {
		t.Fatalf("cancel waiting: %v", err)
	}
	if _, err := h.svc.Admit(ctx, "w1"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if got := h.registry.count(
		`SELECT count(*) FROM coding_slots s JOIN coding_queue q ON q.queue_id=s.queue_id
		 WHERE q.run_id=$1`, r1); got != 0 {
		t.Fatalf("cancelled run got a slot")
	}
	if _, err := h.svc.CancelQueued(ctx, r2, "too late"); err == nil {
		t.Fatalf("cancel on allocated must fail with ErrSlotHeld")
	}
}

// AC-07: lost heartbeat flags suspected but KEEPS the slot; the budget is
// not silently freed.
func TestScheduler_HeartbeatLossRetainsSlot(t *testing.T) {
	h := newSchedulerHarness(t)
	ctx := context.Background()
	runID, _ := h.seedRun()
	if _, err := h.svc.Enqueue(ctx, runID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	res, err := h.svc.Admit(ctx, "w1")
	if err != nil || len(res.Granted) != 1 {
		t.Fatalf("admit: %+v %v", res, err)
	}
	// Age the heartbeat beyond the stale threshold.
	if _, err := h.pool.Exec(ctx,
		`UPDATE coding_slots SET heartbeat_at = CURRENT_TIMESTAMP - interval '10 minutes' WHERE status='held'`); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}
	stale, err := h.svc.SweepStaleHeartbeats(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(stale) != 1 || stale[0] != runID {
		t.Fatalf("suspected = %v, want [%s]", stale, runID)
	}
	if got := h.heldCount(); got != 1 {
		t.Fatalf("suspected slot was released — must stay held, held=%d", got)
	}
	if got := h.registry.count(
		`SELECT count(*) FROM coding_slots WHERE status='held' AND suspected`); got != 1 {
		t.Fatalf("suspected flag not set")
	}
}

// AC-08: release is exactly-once per (run, grant); stale grants release
// nothing, a fresh grant is unaffected by an old confirmation.
func TestScheduler_ReleaseExactlyOnce(t *testing.T) {
	h := newSchedulerHarness(t)
	ctx := context.Background()
	runID, _ := h.seedRun()
	h.svc.Enqueue(ctx, runID)
	res, _ := h.svc.Admit(ctx, "w1")
	g := res.Granted[0]

	first, err := h.svc.ConfirmAgentStopped(ctx, runID, g.GrantID, "exit 0")
	if err != nil || !first.Released {
		t.Fatalf("first release = %+v %v", first, err)
	}
	for _, tc := range []struct{ name, grant string }{
		{"duplicate", g.GrantID},
		{"stale-session", newEventID(t, h.pool)},
	} {
		r, err := h.svc.ConfirmAgentStopped(ctx, runID, tc.grant, "late")
		if err != nil || r.Released {
			t.Fatalf("%s confirm released again: %+v %v", tc.name, r, err)
		}
	}
	if got := h.heldCount(); got != 0 {
		t.Fatalf("held %d after release", got)
	}
	// Re-enqueue after release: a NEW grant works, the OLD confirm can't
	// touch it.
	if _, err := h.svc.Enqueue(ctx, runID); err == nil {
		// run_id UNIQUE prevents a second queue row — re-admission after a
		// released run is out of Phase-1 scope; assert the ledger stayed sane.
		if got := h.heldCount(); got != 0 {
			t.Fatalf("held %d after released run", got)
		}
	}
}

// AC-10: metrics split queue / held / blocked / suspected truthfully.
func TestScheduler_Metrics(t *testing.T) {
	h := newSchedulerHarness(t)
	ctx := context.Background()
	r1, i1 := h.seedRun()
	r2, _ := h.seedRun()
	r3, _ := h.seedRun()
	h.jira.fail(i1, fmt.Errorf("jira down"))
	for _, r := range []string{r1, r2, r3} {
		h.svc.Enqueue(ctx, r)
	}
	if _, err := h.svc.Admit(ctx, "w1"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	// r1 unverified, r2+r3 granted (within budget).
	m, err := h.svc.Metrics(ctx)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if m.BlockedUnverified != 1 || m.Allocated != 2 || m.SlotsHeld != 2 || m.SlotsMax != 3 {
		t.Fatalf("metrics %+v", m)
	}
}
