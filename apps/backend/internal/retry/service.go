package retry

import (
	"context"
	"errors"
	"fmt"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"log/slog"
	"time"
)

type Store interface {
	DueForRetry(context.Context, time.Time, int32) ([]application.RunSnapshot, error)
	ExpiredLeaseRuns(context.Context, time.Time, int32) ([]application.RunSnapshot, error)
	RedriveDelivery(context.Context, string, string, string, string, string) (bool, error)
}
type Transitioner interface {
	TransitionRun(context.Context, application.TransitionCommand) (application.RunSnapshot, error)
}
type Service struct {
	Store    Store
	Registry Transitioner
	Clock    application.Clock
	Logger   *slog.Logger
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now()
}
func (s *Service) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}
func (s *Service) HandleFailure(ctx context.Context, run application.RunSnapshot, cls FailureClass, cause error) (application.RunSnapshot, error) {
	reason := string(cls)
	if cause != nil {
		reason = cause.Error()
	}
	return s.Registry.TransitionRun(ctx, application.TransitionCommand{RunID: run.RunID, ExpectedVersion: run.Version, ExpectedLeaseEpoch: run.LeaseEpoch, Action: domain.ActionScheduleRetry, Actor: domain.ActorPolicy, ActorType: domain.ActorTypeSystem, Reason: reason, FailureClass: string(cls)})
}
func (s *Service) SweepOnce(ctx context.Context, limit int32) error {
	if limit <= 0 {
		limit = 20
	}
	now := s.now()
	expired, err := s.Store.ExpiredLeaseRuns(ctx, now, limit)
	if err != nil {
		return fmt.Errorf("scan expired leases: %w", err)
	}
	due, err := s.Store.DueForRetry(ctx, now, limit)
	if err != nil {
		return fmt.Errorf("scan due retries: %w", err)
	}
	for _, r := range expired {
		if r.State.IsTerminal() || r.State == domain.StateQueued || r.State == domain.StateBlocked || r.State == domain.StateRetryWait {
			continue
		}
		cmd := application.TransitionCommand{RunID: r.RunID, ExpectedVersion: r.Version, ExpectedLeaseEpoch: r.LeaseEpoch, Action: domain.ActionScheduleRetry, Actor: domain.ActorPolicy, ActorType: domain.ActorTypeSystem, Reason: "worker lease expired", FailureClass: string(ClassLeaseLost), RecoverExpiredLease: true}
		if err := s.apply(ctx, cmd); err != nil {
			return err
		}
	}
	for _, r := range due {
		if r.State != domain.StateRetryWait {
			continue
		}
		if err := s.apply(ctx, application.TransitionCommand{RunID: r.RunID, ExpectedVersion: r.Version, ExpectedLeaseEpoch: r.LeaseEpoch, Action: domain.ActionRetryDue, Actor: domain.ActorPolicy, ActorType: domain.ActorTypeSystem, Reason: "retry due"}); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) apply(ctx context.Context, cmd application.TransitionCommand) error {
	_, err := s.Registry.TransitionRun(ctx, cmd)
	if errors.Is(err, domain.ErrStaleVersion) || errors.Is(err, domain.ErrAlreadyTerminal) || errors.Is(err, domain.ErrStaleLease) {
		s.log().Info("retry sweep skipped", "metric", "retry_sweep_skip", "run_id", cmd.RunID, "reason", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("retry sweep run %s: %w", cmd.RunID, err)
	}
	return nil
}
func (s *Service) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.SweepOnce(ctx, 20); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log().Error("retry sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
