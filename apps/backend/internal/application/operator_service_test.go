package application

import (
	"context"
	"testing"
	"time"

	"github.com/vtx/platform/internal/domain"
)

type operatorClock struct{ now time.Time }

func (c operatorClock) Now() time.Time { return c.now }

type fakeOperatorStore struct{ workers []WorkerHeartbeatView }

func (*fakeOperatorStore) RunStateHistogram(context.Context) (map[domain.State]int64, error) {
	return map[domain.State]int64{domain.StateQueued: 2}, nil
}
func (*fakeOperatorStore) CountDeferredIntents(context.Context) (int64, error) { return 3, nil }
func (*fakeOperatorStore) CountActiveRuns(context.Context) (int64, error)      { return 2, nil }
func (*fakeOperatorStore) ListRuns(context.Context, RunFilter) ([]OperatorRunView, error) {
	return nil, nil
}
func (*fakeOperatorStore) GetRunDetail(context.Context, string) (RunDetailView, error) {
	return RunDetailView{}, nil
}
func (*fakeOperatorStore) OutboxStats(context.Context) ([]OutboxTypeStat, error) {
	return []OutboxTypeStat{{Pending: 4, Retrying: 1, DeadLettered: 2, OldestAgeSeconds: 60}}, nil
}
func (*fakeOperatorStore) ListDeadLettered(context.Context, *string, int32) ([]DlqItem, error) {
	return nil, nil
}
func (f *fakeOperatorStore) ListWorkerHeartbeats(context.Context) ([]WorkerHeartbeatView, error) {
	return append([]WorkerHeartbeatView(nil), f.workers...), nil
}
func (*fakeOperatorStore) RedriveDeadLetter(context.Context, string, OperatorAuditRecord) error {
	return nil
}

func TestOperatorOverviewUsesMeasuredRowsAndHeartbeatStaleness(t *testing.T) {
	now := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	store := &fakeOperatorStore{workers: []WorkerHeartbeatView{{WorkerID: "fresh", LastBeatAt: now.Add(-10 * time.Second)}, {WorkerID: "stale", LastBeatAt: now.Add(-46 * time.Second)}}}
	svc := NewOperatorService(store, operatorClock{now}, 15*time.Second, 3)
	got, err := svc.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.RunsByState[domain.StateQueued] != 2 || got.DeferredCount != 3 || got.Admission.Max != 3 || got.Admission.Active != 2 {
		t.Fatalf("overview counts=%#v", got)
	}
	if got.Outbox.Pending != 4 || got.Outbox.Retrying != 1 || got.Outbox.DeadLettered != 2 || got.Workers.Up != 1 || got.Workers.Down != 1 {
		t.Fatalf("overview measured data=%#v", got)
	}
}
