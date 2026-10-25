// Package scheduler owns admission, fencing recovery, and deferred FIFO promotion.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
)

const MaxAdmissionSlots = 3

// Store serializes the ledger across replicas and rolls back the entire callback
// on error. Tx must use the same database transaction for every mutation.
type Store interface {
	Transaction(context.Context, func(Tx) error) error
	Heartbeat(context.Context, string, int) error
}
type Tx interface {
	LiveSlots(context.Context) (int, error)
	Queued(context.Context, int) ([]application.RunSnapshot, error)
	Grant(context.Context, string, string, time.Time) (int64, error)
	Transition(context.Context, application.TransitionCommand) (application.RunSnapshot, error)
	Emit(context.Context, application.RunSnapshot, string, map[string]any) error
	Promote(context.Context, time.Time) error
	Expired(context.Context) ([]application.RunSnapshot, error)
	Due(context.Context) ([]application.RunSnapshot, error)
	Release(context.Context, application.RunSnapshot) error
	SetError(context.Context, string, string) error
}

type Eligibility interface {
	Eligible(context.Context, string) (bool, error)
}
type Config struct {
	Owner                        string
	Tick, LeaseTTL, BeatInterval time.Duration
	JiraGuard                    bool
}

func (c Config) defaults() Config {
	if c.Owner == "" {
		c.Owner = "scheduler-01"
	}
	if c.Tick <= 0 {
		c.Tick = 5 * time.Second
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 120 * time.Second
	}
	if c.BeatInterval <= 0 {
		c.BeatInterval = 15 * time.Second
	}
	return c
}

type Service struct {
	store    Store
	cfg      Config
	clock    application.Clock
	guard    Eligibility
	log      *slog.Logger
	mu       sync.Mutex
	// Admission failure counts are process-local and reset on replica restart.
	failures map[string]int
}

func New(store Store, cfg Config, clock application.Clock, guard Eligibility, log *slog.Logger) *Service {
	if clock == nil {
		clock = application.SystemClock{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, cfg: cfg.defaults(), clock: clock, guard: guard, log: log, failures: map[string]int{}}
}
func command(r application.RunSnapshot, action domain.Action, actor domain.Actor) application.TransitionCommand {
	return application.TransitionCommand{RunID: r.RunID, ExpectedVersion: r.Version, ExpectedLeaseEpoch: r.LeaseEpoch, Action: action, Actor: actor, ActorType: domain.ActorTypeSystem}
}

// Tick is safe to replay from either a dispatcher delivery or the fallback loop.
func (s *Service) Tick(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	var failedRun string
	err := s.store.Transaction(ctx, func(tx Tx) error {
		expired, err := tx.Expired(ctx)
		if err != nil {
			return err
		}
		for _, r := range expired {
			// Clear only the fenced expired holder before policy bookkeeping; a
			// blocked lease_lost run continues counting even without a live lease.
			if err := tx.Release(ctx, r); err != nil {
				return err
			}
			if r.State != domain.StateBlocked {
				if _, err := tx.Transition(ctx, command(r, domain.ActionBlock, domain.ActorPolicy)); err != nil {
					return err
				}
			}
			if err := tx.SetError(ctx, r.RunID, "lease_lost"); err != nil {
				return err
			}
			if err := tx.Emit(ctx, r, "run.lease_lost.v1", map[string]any{"lease_owner": *r.LeaseOwner, "lease_epoch": r.LeaseEpoch, "expired_at": r.LeaseExpiresAt, "action": "blocked", "occurred_at": now}); err != nil {
				return err
			}
		}
		due, err := tx.Due(ctx)
		if err != nil {
			return err
		}
		live, err := tx.LiveSlots(ctx)
		if err != nil {
			return err
		}
		for _, r := range due {
            alreadyCounted := r.LeaseExpiresAt != nil && r.LeaseExpiresAt.After(now)
            if live >= MaxAdmissionSlots && !alreadyCounted {
                continue
            }
			if _, err := tx.Transition(ctx, command(r, domain.ActionRetryDue, domain.ActorScheduler)); err != nil {
				return err
			}
			live, err = tx.LiveSlots(ctx)
			if err != nil {
				return err
			}
		}
		if err := tx.Promote(ctx, now); err != nil {
			return err
		}
		live, err = tx.LiveSlots(ctx)
		if err != nil {
			return err
		}
		if live >= MaxAdmissionSlots {
			return nil
		}
		runs, err := tx.Queued(ctx, 100)
		if err != nil {
			return err
		}
		for _, r := range runs {
			if live >= MaxAdmissionSlots {
				break
			}
			if s.cfg.JiraGuard {
				if s.guard == nil {
					continue
				}
				eligible, err := s.guard.Eligible(ctx, r.RunID)
				if err != nil || !eligible {
					s.log.Warn("scheduler Jira guard skipped", "run_id", r.RunID, "correlation_id", r.CorrelationID)
					continue
				}
			}
			if s.failures[r.RunID] >= 3 {
				if _, err := tx.Transition(ctx, command(r, domain.ActionBlock, domain.ActorPolicy)); err != nil {
					return err
				}
				if err := tx.SetError(ctx, r.RunID, "admission_failed"); err != nil {
					return err
				}
				continue
			}
			epoch, err := tx.Grant(ctx, r.RunID, s.cfg.Owner, now.Add(s.cfg.LeaseTTL))
			if err != nil {
				failedRun = r.RunID
				return err
			}
			r.LeaseEpoch = epoch
			if _, err := tx.Transition(ctx, command(r, domain.ActionBegin, domain.ActorScheduler)); err != nil {
				failedRun = r.RunID
				return err
			}
			live++
			if err := tx.Emit(ctx, r, "task_run.admitted.v1", map[string]any{"lease_owner": s.cfg.Owner, "lease_epoch": epoch, "lease_expires_at": now.Add(s.cfg.LeaseTTL), "live_slots": live, "occurred_at": now}); err != nil {
				return err
			}
			delete(s.failures, r.RunID)
		}
		return nil
	})
	if failedRun != "" {
		s.failures[failedRun]++
	}
	if err != nil {
		return fmt.Errorf("scheduler tick: %w", err)
	}
	return nil
}
func (s *Service) Beat(ctx context.Context) error {
	var active int
	if err := s.store.Transaction(ctx, func(tx Tx) error { var err error; active, err = tx.LiveSlots(ctx); return err }); err != nil {
		return err
	}
	return s.store.Heartbeat(ctx, s.cfg.Owner, active)
}
func (s *Service) Run(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Tick)
	defer tick.Stop()
	beat := time.NewTicker(s.cfg.BeatInterval)
	defer beat.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("scheduler pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case <-beat.C:
			if err := s.Beat(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("scheduler heartbeat failed", "error", err)
			}
		}
	}
}
