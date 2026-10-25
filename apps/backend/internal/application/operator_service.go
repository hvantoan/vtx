package application

import (
	"context"
	"fmt"
	"time"
)

type OperatorService struct {
	store      OperatorStore
	clock      Clock
	staleAfter time.Duration
}

func NewOperatorService(store OperatorStore, clock Clock, heartbeatInterval time.Duration, staleFactor int) *OperatorService {
	if clock == nil {
		clock = SystemClock{}
	}
	if heartbeatInterval <= 0 {
		heartbeatInterval = 15 * time.Second
	}
	if staleFactor <= 0 {
		staleFactor = 3
	}
	return &OperatorService{store: store, clock: clock, staleAfter: heartbeatInterval * time.Duration(staleFactor)}
}
func (s *OperatorService) Overview(ctx context.Context) (OperatorOverview, error) {
	var out OperatorOverview
	var err error
	out.RunsByState, err = s.store.RunStateHistogram(ctx)
	if err != nil {
		return out, err
	}
	out.DeferredCount, err = s.store.CountDeferredIntents(ctx)
	if err != nil {
		return out, err
	}
	out.Admission.Max = 3
	out.Admission.Active, err = s.store.CountActiveRuns(ctx)
	if err != nil {
		return out, err
	}
	stats, err := s.store.OutboxStats(ctx)
	if err != nil {
		return out, err
	}
	for _, v := range stats {
		out.Outbox.Pending += v.Pending
		out.Outbox.Retrying += v.Retrying
		out.Outbox.DeadLettered += v.DeadLettered
		if v.OldestAgeSeconds > out.Outbox.OldestAgeSeconds {
			out.Outbox.OldestAgeSeconds = v.OldestAgeSeconds
		}
	}
	workers, err := s.Workers(ctx)
	if err != nil {
		return out, err
	}
	for _, w := range workers {
		if w.Status == "up" {
			out.Workers.Up++
		} else {
			out.Workers.Down++
		}
	}
	return out, nil
}
func (s *OperatorService) ListRuns(ctx context.Context, f RunFilter) ([]OperatorRunView, error) {
	return s.store.ListRuns(ctx, f)
}
func (s *OperatorService) GetRunDetail(ctx context.Context, id string) (RunDetailView, error) {
	return s.store.GetRunDetail(ctx, id)
}
func (s *OperatorService) OutboxStats(ctx context.Context) ([]OutboxTypeStat, error) {
	return s.store.OutboxStats(ctx)
}
func (s *OperatorService) ListDeadLettered(ctx context.Context, after *string, limit int32) ([]DlqItem, error) {
	return s.store.ListDeadLettered(ctx, after, limit)
}
func (s *OperatorService) Workers(ctx context.Context) ([]WorkerHeartbeatView, error) {
	rows, err := s.store.ListWorkerHeartbeats(ctx)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	for i := range rows {
		rows[i].Status = "up"
		if now.Sub(rows[i].LastBeatAt) > s.staleAfter {
			rows[i].Status = "down"
		}
	}
	return rows, nil
}
func (s *OperatorService) Redrive(ctx context.Context, id string, a OperatorAuditRecord) error {
	if id == "" {
		return fmt.Errorf("%w: missing id", ErrNotDeadLettered)
	}
	a.Action = "dlq.redrive"
	a.TargetType = "outbox_event"
	a.TargetID = id
	return s.store.RedriveDeadLetter(ctx, id, a)
}
