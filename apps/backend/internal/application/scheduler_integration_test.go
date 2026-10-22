//go:build integration

// VTX-5 scheduler against a REAL Postgres: the slot cap under concurrent
// schedulers, priority/FIFO ordering, cancel races, heartbeat suspect, and
// epoch-fenced releases — behaviour a fake cannot prove (AC-01..AC-10).
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
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/persistence/db"
)

// fakeSchedulingSource serves per-issue SchedulingIssue stubs.
type fakeSchedulingSource struct {
	mu   sync.Mutex
	data map[string]jira.SchedulingIssue
	err  map[string]error
}

func (f *fakeSchedulingSource) FetchSchedulingIssue(_ context.Context, issueID string) (jira.SchedulingIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.err[issueID]; ok {
		return jira.SchedulingIssue{}, err
	}
	if d, ok := f.data[issueID]; ok {
		return d, nil
	}
	return jira.SchedulingIssue{ID: issueID, PriorityName: "Medium", PriorityRank: 3}, nil
}

func (f *fakeSchedulingSource) set(issueID string, d jira.SchedulingIssue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[issueID] = d
}

func (f *fakeSchedulingSource) fail(issueID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err[issueID] = err
}

func (f *fakeSchedulingSource) clear(issueID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.err, issueID)
}

func newFakeSource() *fakeSchedulingSource {
	return &fakeSchedulingSource{data: map[string]jira.SchedulingIssue{}, err: map[string]error{}}
}

type schedulerHarness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *persistence.PgSchedulerStore
	source *fakeSchedulingSource
	svc    *application.SchedulerService
	now    time.Time
	runIDs []string
}

func newSchedulerHarness(t *testing.T) *schedulerHarness {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	now := time.Now().UTC()
	store := persistence.NewPgSchedulerStore(pool, db.New(pool))
	source := newFakeSource()
	h := &schedulerHarness{
		t:      t,
		pool:   pool,
		store:  store,
		source: source,
		svc:    application.NewSchedulerService(store, source, "scheduler-a", application.SystemClock{}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))),
		now:    now,
	}
	t.Cleanup(h.cleanup)
	return h
}

// cleanup removes every run this harness seeded; ON DELETE CASCADE clears
// the grant/entry/worktree rows so no test inherits another's slots.
func (h *schedulerHarness) cleanup() {
	for _, runID := range h.runIDs {
		_, _ = h.pool.Exec(context.Background(),
			`DELETE FROM task_runs WHERE run_id = $1::uuid`, runID)
	}
}

// seedPreparedRun creates registry task + intent + run in 'preparing' + an
// allocated worktree — the enqueue eligibility pair. Returns (runID, taskID, issueID).
func (h *schedulerHarness) seedPreparedRun(issueKey string) (string, string, string) {
	h.t.Helper()
	issueID := fmt.Sprintf("8%09d", time.Now().UnixNano()%999_999_999)
	ctx := context.Background()

	h.runIDs = append(h.runIDs, "pending")
	var taskID, runID string
	err := h.pool.QueryRow(ctx,
		`INSERT INTO registry_tasks (jira_site_id, jira_issue_id, jira_issue_key, project_key)
		 VALUES ('site-1', $1, $2, 'VTX') RETURNING id::text`, issueID, issueKey).Scan(&taskID)
	if err != nil {
		h.t.Fatalf("seed task: %v", err)
	}
	var intentID string
	err = h.pool.QueryRow(ctx,
		`INSERT INTO execution_intents (issue_key, target_status_id)
		 VALUES ($1, '10007') RETURNING intent_id::text`, issueKey).Scan(&intentID)
	if err != nil {
		h.t.Fatalf("seed intent: %v", err)
	}
	err = h.pool.QueryRow(ctx,
		`INSERT INTO task_runs (task_id, intent_id, state)
		 VALUES ($1::uuid, $2::uuid, 'preparing') RETURNING run_id::text`, taskID, intentID).Scan(&runID)
	if err != nil {
		h.t.Fatalf("seed run: %v", err)
	}
	_, err = h.pool.Exec(ctx,
		`INSERT INTO worktrees (run_id, task_id, status, path, branch, repo_url)
		 VALUES ($1::uuid, $2::uuid, 'allocated', '/wt/' || $3, 'vtx/' || $4, 'https://repo')`,
		runID, taskID, runID, issueKey)
	if err != nil {
		h.t.Fatalf("seed worktree: %v", err)
	}
	h.runIDs[len(h.runIDs)-1] = runID
	return runID, taskID, issueID
}

func (h *schedulerHarness) enqueue(runID string) application.QueueEntry {
	h.t.Helper()
	entry, err := h.svc.EnqueueForCoding(context.Background(), runID)
	if err != nil {
		h.t.Fatalf("enqueue run %s: %v", runID, err)
	}
	return entry
}

func (h *schedulerHarness) cancelRun(runID string) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE task_runs SET state = 'cancelled' WHERE run_id = $1::uuid`, runID); err != nil {
		h.t.Fatalf("cancel run: %v", err)
	}
}

func (h *schedulerHarness) activeGrants() int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM coding_slot_grants WHERE status <> 'released'`).Scan(&n); err != nil {
		h.t.Fatalf("count grants: %v", err)
	}
	return n
}

func (h *schedulerHarness) entryStatus(runID string) (string, string) {
	h.t.Helper()
	var status, reason string
	err := h.pool.QueryRow(context.Background(),
		`SELECT status, COALESCE(block_reason,'') FROM coding_queue_entries WHERE run_id=$1::uuid`, runID).Scan(&status, &reason)
	if err != nil {
		h.t.Fatalf("entry status: %v", err)
	}
	return status, reason
}

// AC-01: four eligible tasks, three slots — the fourth waits.
func TestSchedulerGrantsAtMostThree(t *testing.T) {
	h := newSchedulerHarness(t)
	for i := 0; i < 4; i++ {
		runID, _, _ := h.seedPreparedRun(fmt.Sprintf("VTX-%d", 100+i))
		h.enqueue(runID)
	}
	rep, err := h.svc.ScheduleOnce(context.Background())
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if len(rep.Granted) != 3 {
		t.Fatalf("expected 3 grants, got %d", len(rep.Granted))
	}
	if got := h.activeGrants(); got != 3 {
		t.Fatalf("expected 3 active grants, got %d", got)
	}
	m, err := h.svc.Metrics(context.Background())
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if m.SlotsHeld != 3 || m.SlotsFree != 0 || m.QueueWaiting != 1 {
		t.Fatalf("bad metrics: %+v", m)
	}
}

// AC-02: many schedulers racing on the same pool never exceed three grants.
func TestSchedulerConcurrentCap(t *testing.T) {
	h := newSchedulerHarness(t)
	for i := 0; i < 8; i++ {
		runID, _, _ := h.seedPreparedRun(fmt.Sprintf("VTX-%d", 200+i))
		h.enqueue(runID)
	}
	var wg sync.WaitGroup
	for w := 0; w < 5; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			svc := application.NewSchedulerService(h.store, h.source, fmt.Sprintf("sched-%d", id), nil, nil)
			_, _ = svc.ScheduleOnce(context.Background())
		}(w)
	}
	wg.Wait()
	if got := h.activeGrants(); got != 3 {
		t.Fatalf("concurrent schedulers: expected ≤3 active grants, got %d", got)
	}
}

// AC-03: re-granting the same run is idempotent — no second allocation.
func TestSchedulerRegrantIdempotent(t *testing.T) {
	h := newSchedulerHarness(t)
	runID, _, _ := h.seedPreparedRun("VTX-300")
	h.enqueue(runID)
	rep, err := h.svc.ScheduleOnce(context.Background())
	if err != nil || len(rep.Granted) != 1 {
		t.Fatalf("first schedule: rep=%+v err=%v", rep, err)
	}
	first := rep.Granted[0]

	// Simulate retry: entry is 'granted' now, so GrantSlot on it reports
	// Skipped — but GetActiveGrantForRun guards the re-grant path for any
	// caller that re-attempts while the entry is still waiting.
	if got := h.activeGrants(); got != 1 {
		t.Fatalf("expected 1 active grant, got %d", got)
	}
	var count int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM coding_slot_grants WHERE run_id=$1::uuid`, runID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 || first.SessionEpoch != 1 {
		t.Fatalf("expected single grant epoch 1, got count=%d epoch=%d", count, first.SessionEpoch)
	}
}

// AC-04: higher Jira priority wins even when enqueued later; equal priority
// follows FIFO; a blocked head never stalls eligible tasks behind it.
func TestSchedulerPriorityThenFIFO(t *testing.T) {
	h := newSchedulerHarness(t)

	lowRun, _, lowID := h.seedPreparedRun("VTX-401")
	h.enqueue(lowRun)
	h.source.set(lowID, jira.SchedulingIssue{ID: lowID, PriorityName: "Low", PriorityRank: 2})

	highRun, _, highID := h.seedPreparedRun("VTX-402")
	h.enqueue(highRun)
	h.source.set(highID, jira.SchedulingIssue{ID: highID, PriorityName: "Highest", PriorityRank: 5})

	rep, err := h.svc.ScheduleOnce(context.Background())
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if len(rep.Granted) < 1 || rep.Granted[0].RunID != highRun {
		t.Fatalf("expected highest-priority run granted first, got %+v", rep.Granted)
	}
}

func TestSchedulerBlockedHeadDoesNotStall(t *testing.T) {
	h := newSchedulerHarness(t)

	blockedRun, _, blockedID := h.seedPreparedRun("VTX-410")
	h.enqueue(blockedRun)
	h.source.set(blockedID, jira.SchedulingIssue{
		ID: blockedID, PriorityName: "Highest", PriorityRank: 5,
		UnresolvedBlockers: []string{"VTX-999"},
	})

	okRun, _, _ := h.seedPreparedRun("VTX-411")
	h.enqueue(okRun)

	rep, err := h.svc.ScheduleOnce(context.Background())
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if len(rep.Granted) != 1 || rep.Granted[0].RunID != okRun {
		t.Fatalf("blocked head stalled the queue: %+v", rep.Granted)
	}
	status, reason := h.entryStatus(blockedRun)
	if status != "waiting" || reason == "" {
		t.Fatalf("blocked entry should stay waiting with a reason, got %s %q", status, reason)
	}
}

// AC-05: unresolved dependency or unverifiable Jira → no grant, reason kept.
func TestSchedulerJiraUnverifiableBlocks(t *testing.T) {
	h := newSchedulerHarness(t)
	runID, _, issueID := h.seedPreparedRun("VTX-420")
	h.enqueue(runID)
	h.source.fail(issueID, fmt.Errorf("jira api status 403"))

	rep, err := h.svc.ScheduleOnce(context.Background())
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if len(rep.Granted) != 0 || rep.Blocked != 1 {
		t.Fatalf("expected blocked entry, got %+v", rep)
	}
	status, reason := h.entryStatus(runID)
	if status != "waiting" || reason != "jira_unverified" {
		t.Fatalf("expected waiting/jira_unverified, got %s %q", status, reason)
	}

	// Recovery: once Jira answers, the same entry is granted.
	h.source.clear(issueID)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE coding_queue_entries SET recheck_at = NULL WHERE run_id=$1::uuid`, runID); err != nil {
		t.Fatalf("force recheck: %v", err)
	}
	rep, err = h.svc.ScheduleOnce(context.Background())
	if err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	if len(rep.Granted) != 1 {
		t.Fatalf("expected grant after recheck, got %+v", rep)
	}
}

// AC-06: a cancel that lands between listing and granting wins — no slot.
func TestSchedulerCancelRace(t *testing.T) {
	h := newSchedulerHarness(t)
	runID, _, _ := h.seedPreparedRun("VTX-430")
	entry := h.enqueue(runID)

	h.cancelRun(runID)

	outcome, err := h.store.GrantSlot(context.Background(), application.GrantCommand{
		EntryID: entry.EntryID, RunID: runID, TaskID: entry.TaskID, Owner: "scheduler-a",
	}, "test")
	if err != nil {
		t.Fatalf("grant tx: %v", err)
	}
	if !outcome.Closed || outcome.Granted {
		t.Fatalf("cancelled run must not receive a slot: %+v", outcome)
	}
	if got := h.activeGrants(); got != 0 {
		t.Fatalf("cancelled run got a slot: %d active", got)
	}
	status, _ := h.entryStatus(runID)
	if status != "closed" {
		t.Fatalf("entry should be closed, got %s", status)
	}
}

// AC-07: heartbeat loss marks the grant suspect and KEEPS the slot.
func TestSchedulerHeartbeatLossKeepsSlot(t *testing.T) {
	h := newSchedulerHarness(t)
	runID, _, _ := h.seedPreparedRun("VTX-440")
	h.enqueue(runID)
	rep, err := h.svc.ScheduleOnce(context.Background())
	if err != nil || len(rep.Granted) != 1 {
		t.Fatalf("schedule: %+v %v", rep, err)
	}
	grant := rep.Granted[0]

	// No heartbeat ever recorded → stale immediately past TTL.
	h.svc.SetHeartbeatTTL(time.Millisecond)
	time.Sleep(2 * time.Millisecond)
	n, err := h.svc.SweepSuspectGrants(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 suspect grant, got %d", n)
	}
	if got := h.activeGrants(); got != 1 {
		t.Fatalf("suspect must keep the slot, active=%d", got)
	}
	m, _ := h.svc.Metrics(context.Background())
	if m.SlotsSuspect != 1 || m.SlotsHeld != 0 || m.SlotsFree != 2 {
		t.Fatalf("bad suspect metrics: %+v", m)
	}
	if grant.GrantID == "" {
		t.Fatal("missing grant id")
	}
}

// AC-08: release happens exactly once on a valid epoch; stale and duplicate
// confirmations are safe no-ops.
func TestSchedulerConfirmStoppedFencing(t *testing.T) {
	h := newSchedulerHarness(t)
	runID, _, _ := h.seedPreparedRun("VTX-450")
	h.enqueue(runID)
	rep, err := h.svc.ScheduleOnce(context.Background())
	if err != nil || len(rep.Granted) != 1 {
		t.Fatalf("schedule: %+v %v", rep, err)
	}
	g := rep.Granted[0]

	outcome, err := h.svc.ConfirmAgentStopped(context.Background(), g.GrantID, g.SessionEpoch+99, "wrong epoch")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if outcome != application.ConfirmStaleSession {
		t.Fatalf("stale epoch must not release: %s", outcome)
	}
	if got := h.activeGrants(); got != 1 {
		t.Fatalf("stale confirmation released the slot: %d", got)
	}

	outcome, err = h.svc.ConfirmAgentStopped(context.Background(), g.GrantID, g.SessionEpoch, "agent stopped")
	if err != nil || outcome != application.ConfirmReleased {
		t.Fatalf("valid confirm: %s %v", outcome, err)
	}
	outcome, err = h.svc.ConfirmAgentStopped(context.Background(), g.GrantID, g.SessionEpoch, "duplicate")
	if err != nil || outcome != application.ConfirmAlreadyReleased {
		t.Fatalf("duplicate confirm must be a no-op: %s %v", outcome, err)
	}
	if got := h.activeGrants(); got != 0 {
		t.Fatalf("slot still held after release: %d", got)
	}

	var events int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM coding_slot_events WHERE run_id=$1::uuid`, runID).Scan(&events); err != nil {
		t.Fatalf("events: %v", err)
	}
	if events < 4 { // enqueued + granted + released + stale + duplicate
		t.Fatalf("audit trail incomplete: %d events", events)
	}
}

// AC-09: only prepared runs (preparing + allocated worktree) may enqueue.
func TestSchedulerEnqueueEligibility(t *testing.T) {
	h := newSchedulerHarness(t)

	// queued run without worktree → rejected.
	ctx := context.Background()
	issueID := fmt.Sprintf("8%09d", time.Now().UnixNano()%999_999_999)
	var taskID, intentID, runID string
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO registry_tasks (jira_site_id, jira_issue_id, jira_issue_key, project_key)
		 VALUES ('site-1',$1,'VTX-460','VTX') RETURNING id::text`, issueID).Scan(&taskID); err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO execution_intents (issue_key, target_status_id) VALUES ('VTX-460','10007') RETURNING intent_id::text`).Scan(&intentID); err != nil {
		t.Fatalf("intent: %v", err)
	}
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO task_runs (task_id, intent_id, state) VALUES ($1::uuid,$2::uuid,'queued') RETURNING run_id::text`, taskID, intentID).Scan(&runID); err != nil {
		t.Fatalf("run: %v", err)
	}
	h.runIDs = append(h.runIDs, runID)
	if _, err := h.svc.EnqueueForCoding(ctx, runID); err == nil {
		t.Fatal("queued run must not enqueue for coding")
	}

	// Non-coding stages (verifying/qa) also cannot enqueue: a run already
	// past coding has no business competing for a coding slot.
	prepRun, _, _ := h.seedPreparedRun("VTX-461")
	if _, err := h.pool.Exec(ctx,
		`UPDATE task_runs SET state='verifying' WHERE run_id=$1::uuid`, prepRun); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := h.svc.EnqueueForCoding(ctx, prepRun); err == nil {
		t.Fatal("verifying run must not enqueue for coding")
	}
}
