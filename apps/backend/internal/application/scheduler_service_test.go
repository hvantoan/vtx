package application

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vtx/platform/internal/domain"
)

// fakeSchedulerStore is an in-memory SchedulerStore that honors the same
// guarded-mutation contract the pgx adapter implements: GrantPass re-checks
// status inside a lock, ConfirmStop requires an exact held-grant match, and
// cancel only touches non-allocated entries.
type fakeSchedulerStore struct {
	mu      sync.Mutex
	runs    map[string]ScheduledRunView
	entries map[string]*QueueEntryView // by queue_id
	byRun   map[string]string          // run_id -> queue_id
	slots   map[string]*SlotGrant      // by grant_id
	held    []string                   // grant_ids in grant order
	fifo    int64
	events  []string
}

func newFakeSchedulerStore() *fakeSchedulerStore {
	return &fakeSchedulerStore{
		runs:    map[string]ScheduledRunView{},
		entries: map[string]*QueueEntryView{},
		byRun:   map[string]string{},
		slots:   map[string]*SlotGrant{},
	}
}

func (f *fakeSchedulerStore) addRun(runID, state string) {
	id := fmt.Sprintf("issue-%s", runID)
	f.runs[runID] = ScheduledRunView{
		RunID: runID, TaskID: "task-" + runID, State: domain.State(state),
		JiraSiteID: "site", JiraIssueID: id, JiraIssueKey: "VTX-" + runID,
	}
}

func (f *fakeSchedulerStore) GetRunForSchedule(_ context.Context, runID string) (ScheduledRunView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[runID]
	if !ok {
		return ScheduledRunView{}, ErrScheduledRunNotFound
	}
	return r, nil
}

func (f *fakeSchedulerStore) Enqueue(_ context.Context, e QueueEntryView, _ string) (QueueEntryView, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if qid, ok := f.byRun[e.RunID]; ok {
		return *f.entries[qid], false, nil
	}
	f.fifo++
	e.QueueID = fmt.Sprintf("q-%d", f.fifo)
	e.FifoSeq = f.fifo
	e.Status = domain.QueueWaiting
	f.entries[e.QueueID] = &e
	f.byRun[e.RunID] = e.QueueID
	return e, true, nil
}

func (f *fakeSchedulerStore) ListSchedulable(_ context.Context) ([]QueueEntryView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []QueueEntryView
	for _, e := range f.entries {
		if e.Status == domain.QueueWaiting || e.Status == domain.QueueBlockedDependency || e.Status == domain.QueueBlockedUnverified {
			out = append(out, *e)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].FifoSeq < out[j-1].FifoSeq; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func (f *fakeSchedulerStore) SetQueuePriority(_ context.Context, queueID string, rank int, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.entries[queueID]; ok {
		e.PriorityRank = rank
		e.PriorityName = name
	}
	return nil
}

func (f *fakeSchedulerStore) SetQueueBlocked(_ context.Context, queueID string, status domain.QueueStatus, reason, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[queueID]
	if !ok {
		return fmt.Errorf("%w: %s", domain.ErrQueueNotFound, queueID)
	}
	switch e.Status {
	case domain.QueueWaiting, domain.QueueBlockedDependency, domain.QueueBlockedUnverified:
		e.Status = status
		e.BlockedReason = reason
	}
	return nil
}

func (f *fakeSchedulerStore) GrantPass(_ context.Context, worker string, orderedQueueIDs []string, max int, _ string) ([]SlotGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var grants []SlotGrant
	for _, qid := range orderedQueueIDs {
		if len(f.held) >= max {
			break
		}
		e := f.entries[qid]
		if e == nil || e.Status != domain.QueueWaiting {
			continue
		}
		g := SlotGrant{
			GrantID: fmt.Sprintf("g-%s", qid), QueueID: qid,
			RunID: e.RunID, TaskID: e.TaskID, WorkerOwner: worker,
		}
		e.Status = domain.QueueAllocated
		e.GrantID = g.GrantID
		f.slots[g.GrantID] = &g
		f.held = append(f.held, g.GrantID)
		grants = append(grants, g)
	}
	return grants, nil
}

func (f *fakeSchedulerStore) ConfirmStop(_ context.Context, runID, grantID, _, _ string) (ReleaseResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.slots[grantID]
	if !ok || g.RunID != runID {
		return ReleaseResult{Released: false, RunID: runID, GrantID: grantID}, nil
	}
	// exact-once: remove from held, mark queue released.
	next := f.held[:0]
	for _, id := range f.held {
		if id != grantID {
			next = append(next, id)
		}
	}
	f.held = next
	delete(f.slots, grantID)
	if e := f.entries[g.QueueID]; e != nil && e.Status == domain.QueueAllocated {
		e.Status = domain.QueueReleased
		e.GrantID = ""
	}
	return ReleaseResult{Released: true, GrantID: grantID, QueueID: g.QueueID, RunID: runID}, nil
}

func (f *fakeSchedulerStore) RecordHeartbeat(_ context.Context, runID, grantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.slots[grantID]
	if !ok || g.RunID != runID {
		return fmt.Errorf("%w", domain.ErrStaleGrant)
	}
	return nil
}

func (f *fakeSchedulerStore) MarkStaleSuspected(_ context.Context, _ time.Time) ([]string, error) {
	return nil, nil
}

func (f *fakeSchedulerStore) CancelQueued(_ context.Context, runID, reason, _ string) (QueueEntryView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	qid, ok := f.byRun[runID]
	if !ok {
		return QueueEntryView{}, fmt.Errorf("%w", domain.ErrQueueNotFound)
	}
	e := f.entries[qid]
	if e.Status == domain.QueueAllocated {
		return QueueEntryView{}, fmt.Errorf("%w", domain.ErrSlotHeld)
	}
	e.Status = domain.QueueCancelled
	e.BlockedReason = reason
	return *e, nil
}

func (f *fakeSchedulerStore) Metrics(_ context.Context) (SchedulerMetrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m SchedulerMetrics
	for _, e := range f.entries {
		switch e.Status {
		case domain.QueueWaiting:
			m.Waiting++
		case domain.QueueAllocated:
			m.Allocated++
		case domain.QueueBlockedDependency:
			m.BlockedDependency++
		case domain.QueueBlockedUnverified:
			m.BlockedUnverified++
		case domain.QueueCancelled:
			m.Cancelled++
		case domain.QueueReleased:
			m.Released++
		}
	}
	m.SlotsHeld = int64(len(f.held))
	return m, nil
}

// fakeJira returns canned scheduling data per issue id.
type fakeJira struct {
	mu    sync.Mutex
	data  map[string]SchedulingData
	err   map[string]error
	calls int
}

func newFakeJira() *fakeJira {
	return &fakeJira{data: map[string]SchedulingData{}, err: map[string]error{}}
}

func (f *fakeJira) set(issueID, priority string, blockers ...BlockerRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[issueID] = SchedulingData{PriorityName: priority, BlockedBy: blockers}
}

func (f *fakeJira) fail(issueID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err[issueID] = err
}

func (f *fakeJira) FetchSchedulingData(_ context.Context, issueID string) (SchedulingData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err, ok := f.err[issueID]; ok {
		return SchedulingData{}, err
	}
	if d, ok := f.data[issueID]; ok {
		return d, nil
	}
	return SchedulingData{PriorityName: "Medium"}, nil
}

func newService() (*SchedulerService, *fakeSchedulerStore, *fakeJira) {
	store := newFakeSchedulerStore()
	jira := newFakeJira()
	return NewSchedulerService(store, jira, "scheduler-test", nil, nil), store, jira
}

// AC-01: four eligible tasks arrive together — exactly three get slots.
func TestAdmit_GrantsAtMostThree(t *testing.T) {
	svc, store, _ := newService()
	ctx := context.Background()
	for _, r := range []string{"r1", "r2", "r3", "r4"} {
		store.addRun(r, "preparing")
		if _, err := svc.Enqueue(ctx, r); err != nil {
			t.Fatalf("enqueue %s: %v", r, err)
		}
	}
	res, err := svc.Admit(ctx, "worker-a")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if len(res.Granted) != 3 {
		t.Fatalf("granted %d, want 3", len(res.Granted))
	}
	m, _ := svc.Metrics(ctx)
	if m.SlotsHeld != 3 || m.Waiting != 1 {
		t.Fatalf("metrics %+v, want held=3 waiting=1", m)
	}
}

// AC-04: Jira priority first; FIFO inside one priority; a blocked head does
// not starve eligible entries behind it.
func TestAdmit_PriorityThenFifo(t *testing.T) {
	svc, store, jira := newService()
	ctx := context.Background()
	// Enqueue order: low (first), high-blocked, high-free, low (last).
	for _, r := range []string{"r1", "r2", "r3", "r4"} {
		store.addRun(r, "preparing")
		svc.Enqueue(ctx, r)
	}
	jira.set("issue-r1", "Low")
	jira.set("issue-r2", "High", BlockerRef{IssueID: "9", IssueKey: "VTX-9", StatusName: "In Progress", Done: false})
	jira.set("issue-r3", "High")
	jira.set("issue-r4", "Low")

	res, err := svc.Admit(ctx, "worker-a")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if len(res.Granted) != 3 {
		t.Fatalf("granted %d, want 3 (r3 High + r1/r4 Low; r2 blocked)", len(res.Granted))
	}
	if res.Granted[0].RunID != "r3" {
		t.Fatalf("first grant = %s, want r3 (only eligible High)", res.Granted[0].RunID)
	}
	if res.Granted[1].RunID != "r1" || res.Granted[2].RunID != "r4" {
		t.Fatalf("low grants = %s,%s, want r1,r4 (FIFO inside Low)", res.Granted[1].RunID, res.Granted[2].RunID)
	}
	entry, _ := store.GetQueueEntryByRunForTest(ctx, "r2")
	if entry.Status != domain.QueueBlockedDependency {
		t.Fatalf("r2 status = %s, want blocked_dependency", entry.Status)
	}
}

// GetQueueEntryByRunForTest is a test helper on the fake — production reads
// go through ListSchedulable; the interface stays minimal.
func (f *fakeSchedulerStore) GetQueueEntryByRunForTest(_ context.Context, runID string) (QueueEntryView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	qid, ok := f.byRun[runID]
	if !ok {
		return QueueEntryView{}, fmt.Errorf("%w", domain.ErrQueueNotFound)
	}
	return *f.entries[qid], nil
}

// AC-05: unverifiable Jira read ⇒ no slot; verified-undone blocker ⇒ no slot.
func TestAdmit_DependencyGating(t *testing.T) {
	svc, store, jira := newService()
	ctx := context.Background()
	for _, r := range []string{"r1", "r2", "r3"} {
		store.addRun(r, "preparing")
		svc.Enqueue(ctx, r)
	}
	jira.fail("issue-r1", errors.New("jira 503"))
	jira.set("issue-r2", "Medium", BlockerRef{IssueID: "9", IssueKey: "VTX-9", StatusName: "To Do"})
	// r3 clean.

	res, err := svc.Admit(ctx, "worker-a")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if len(res.Granted) != 1 || res.Granted[0].RunID != "r3" {
		t.Fatalf("only r3 may be granted, got %+v", res.Granted)
	}
	e1, _ := store.GetQueueEntryByRunForTest(ctx, "r1")
	e2, _ := store.GetQueueEntryByRunForTest(ctx, "r2")
	if e1.Status != domain.QueueBlockedUnverified {
		t.Fatalf("r1 = %s, want blocked_unverified", e1.Status)
	}
	if e2.Status != domain.QueueBlockedDependency {
		t.Fatalf("r2 = %s, want blocked_dependency", e2.Status)
	}
}

// AC-06: a cancelled waiting entry can never receive a slot, even when the
// cancel lands between evaluation and the grant pass.
func TestCancel_BlocksGrant(t *testing.T) {
	svc, store, _ := newService()
	ctx := context.Background()
	store.addRun("r1", "preparing")
	svc.Enqueue(ctx, "r1")
	if _, err := svc.CancelQueued(ctx, "r1", "operator cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	res, err := svc.Admit(ctx, "worker-a")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if len(res.Granted) != 0 {
		t.Fatalf("cancelled task must not be granted, got %+v", res.Granted)
	}
}

// AC-06 race: cancel arriving after a grant ⇒ ErrSlotHeld, slot untouched.
func TestCancel_AllocatedIsHeld(t *testing.T) {
	svc, store, _ := newService()
	ctx := context.Background()
	store.addRun("r1", "preparing")
	svc.Enqueue(ctx, "r1")
	res, _ := svc.Admit(ctx, "worker-a")
	if len(res.Granted) != 1 {
		t.Fatalf("expected a grant")
	}
	if _, err := svc.CancelQueued(ctx, "r1", "too late"); !errors.Is(err, domain.ErrSlotHeld) {
		t.Fatalf("cancel on allocated = %v, want ErrSlotHeld", err)
	}
	m, _ := svc.Metrics(ctx)
	if m.SlotsHeld != 1 {
		t.Fatalf("slot must remain held, got %+v", m)
	}
}

// AC-08: exact grant releases once; duplicate/late/stale releases nothing.
func TestConfirmStop_ExactlyOnce(t *testing.T) {
	svc, store, _ := newService()
	ctx := context.Background()
	store.addRun("r1", "preparing")
	svc.Enqueue(ctx, "r1")
	res, _ := svc.Admit(ctx, "worker-a")
	g := res.Granted[0]

	first, err := svc.ConfirmAgentStopped(ctx, "r1", g.GrantID, "agent exited 0")
	if err != nil || !first.Released {
		t.Fatalf("first confirm = %+v err %v", first, err)
	}
	dup, err := svc.ConfirmAgentStopped(ctx, "r1", g.GrantID, "agent exited 0")
	if err != nil || dup.Released {
		t.Fatalf("duplicate confirm released again: %+v", dup)
	}
	stale, err := svc.ConfirmAgentStopped(ctx, "r1", "g-old-session", "late")
	if err != nil || stale.Released {
		t.Fatalf("stale confirm released: %+v", stale)
	}
	m, _ := svc.Metrics(ctx)
	if m.SlotsHeld != 0 || m.Released != 1 {
		t.Fatalf("metrics %+v, want held=0 released=1", m)
	}
}

// AC-03: re-enqueue of the same run is idempotent — never two queue entries,
// so never two concurrent grants for one run.
func TestEnqueue_Idempotent(t *testing.T) {
	svc, store, _ := newService()
	ctx := context.Background()
	store.addRun("r1", "preparing")
	a, err := svc.Enqueue(ctx, "r1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	b, err := svc.Enqueue(ctx, "r1")
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if a.QueueID != b.QueueID {
		t.Fatalf("re-enqueue created a second entry: %s vs %s", a.QueueID, b.QueueID)
	}
}

// Terminal runs never join the queue (AC-09 helpers: only live runs may be
// evaluated at all).
func TestEnqueue_TerminalRejected(t *testing.T) {
	svc, store, _ := newService()
	ctx := context.Background()
	store.addRun("r1", "cancelled")
	if _, err := svc.Enqueue(ctx, "r1"); !errors.Is(err, domain.ErrRunNotEligible) {
		t.Fatalf("enqueue terminal = %v, want ErrRunNotEligible", err)
	}
	store.addRun("r2", "failed")
	if _, err := svc.Enqueue(ctx, "r2"); !errors.Is(err, domain.ErrRunNotEligible) {
		t.Fatalf("enqueue failed = %v, want ErrRunNotEligible", err)
	}
}
