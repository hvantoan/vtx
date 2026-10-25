package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence/db"
	"github.com/vtx/platform/internal/scheduler"
)

type PgSchedulerStore struct {
	pool    txBeginner
	queries *db.Queries
}

func NewPgSchedulerStore(pool txBeginner, q *db.Queries) *PgSchedulerStore {
	return &PgSchedulerStore{pool, q}
}
func (s *PgSchedulerStore) Transaction(ctx context.Context, fn func(scheduler.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin scheduler: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	if err := q.LockAdmissionLedger(ctx); err != nil {
		return fmt.Errorf("lock admission: %w", err)
	}
	// Registry transitions use pgx nested transactions (savepoints), so all
	// lease, state, audit and outbox effects remain inside this outer commit.
	st := &schedulerTx{q: q, registry: application.NewRegistryService(NewPgRegistryStore(tx, q), "scheduler", nil, nil)}
	if err := fn(st); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit scheduler: %w", err)
	}
	return nil
}
func (s *PgSchedulerStore) Heartbeat(ctx context.Context, owner string, active int) error {
	_, err := s.queries.UpsertWorkerHeartbeat(ctx, db.UpsertWorkerHeartbeatParams{WorkerID: owner, WorkerKind: "scheduler", ActiveRuns: int32(active)})
	if err != nil {
		return fmt.Errorf("scheduler heartbeat: %w", err)
	}
	return nil
}

type schedulerTx struct {
	q        *db.Queries
	registry *application.RegistryService
}

func (s *schedulerTx) LiveSlots(ctx context.Context) (int, error) {
	n, err := s.q.CountLiveAdmissionSlots(ctx)
	return int(n), err
}
func snapshots(rows []db.TaskRun) []application.RunSnapshot {
	out := make([]application.RunSnapshot, 0, len(rows))
	for _, r := range rows {
		out = append(out, snapshotFromTaskRun(r))
	}
	return out
}
func (s *schedulerTx) Queued(ctx context.Context, limit int) ([]application.RunSnapshot, error) {
	rows, err := s.q.ListQueuedRunsForAdmission(ctx, int32(limit))
	return snapshots(rows), err
}
func (s *schedulerTx) Expired(ctx context.Context) ([]application.RunSnapshot, error) {
	rows, err := s.q.ListExpiredLeaseRuns(ctx, 100)
	return snapshots(rows), err
}
func (s *schedulerTx) Due(ctx context.Context) ([]application.RunSnapshot, error) {
	rows, err := s.q.ListDueRetryRuns(ctx, 100)
	return snapshots(rows), err
}
func (s *schedulerTx) Grant(ctx context.Context, id, owner string, expiry time.Time) (int64, error) {
	uid, err := uuidFromString(id)
	if err != nil {
		return 0, err
	}
	lease, err := s.q.AcquireRunLease(ctx, db.AcquireRunLeaseParams{RunID: uid, LeaseOwner: textFromOptional(owner), LeaseExpiresAt: pgtype.Timestamptz{Time: expiry, Valid: true}})
	if err != nil {
		return 0, fmt.Errorf("grant lease: %w", err)
	}
	return lease.LeaseEpoch, nil
}
func (s *schedulerTx) Transition(ctx context.Context, cmd application.TransitionCommand) (application.RunSnapshot, error) {
	return s.registry.TransitionRun(ctx, cmd)
}
func (s *schedulerTx) Release(ctx context.Context, r application.RunSnapshot) error {
	uid, err := uuidFromString(r.RunID)
	if err != nil {
		return err
	}
	locked, err := s.q.GetTaskRunForUpdate(ctx, uid)
	if err != nil {
		return err
	}
	if r.LeaseOwner == nil || !locked.LeaseOwner.Valid || locked.LeaseOwner.String != *r.LeaseOwner || locked.LeaseEpoch != r.LeaseEpoch {
		return domain.ErrStaleLease
	}
	return s.q.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{RunID: uid, LeaseOwner: textFrom(r.LeaseOwner), LeaseEpoch: r.LeaseEpoch})
}
func (s *schedulerTx) SetError(ctx context.Context, id, code string) error {
	uid, err := uuidFromString(id)
	if err != nil {
		return err
	}
	return s.q.SetSchedulerRunError(ctx, db.SetSchedulerRunErrorParams{RunID: uid, ErrorCode: textFromOptional(code)})
}
func (s *schedulerTx) Emit(ctx context.Context, r application.RunSnapshot, kind string, body map[string]any) error {
	body["run_id"] = r.RunID
	body["task_id"] = r.TaskID
	body["correlation_id"] = r.CorrelationID
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s: %w", kind, err)
	}
	id, err := uuidFromString(r.IntentID)
	if err != nil {
		return err
	}
	_, err = s.q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{IntentID: id, EventType: kind, Payload: payload})
	if err != nil {
		return fmt.Errorf("emit %s: %w", kind, err)
	}
	return nil
}
func (s *schedulerTx) Promote(ctx context.Context, now time.Time) error {
	tasks, err := s.q.ListPromotableTasks(ctx, 100)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		active, err := s.q.HasActiveRunForTask(ctx, task.ID)
		if err != nil {
			return err
		}
		if active {
			continue
		}
		claim, err := s.q.ClaimDeferredForTask(ctx, task.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		origin, err := s.q.GetDeferredIntentOrigin(ctx, claim.IntentID)
		if err != nil {
			return fmt.Errorf("deferred origin: %w", err)
		}
		row, err := s.q.InsertTaskRun(ctx, db.InsertTaskRunParams{TaskID: task.ID, IntentID: claim.IntentID, OriginEventID: origin.EventID, ChangelogID: pgtype.Text{String: origin.ChangelogID, Valid: true}, CorrelationID: origin.CorrelationID})
		if err != nil {
			return err
		}
		_, err = s.q.InsertTaskRunEvent(ctx, db.InsertTaskRunEventParams{RunID: row.RunID, CurrentState: "queued", Action: "created", Actor: "scheduler", ActorType: "system", CorrelationID: row.CorrelationID, Version: 0})
		if err != nil {
			return err
		}
		run := snapshotFromTaskRun(row)
		if err := s.Emit(ctx, run, application.OutboxEventTaskRunCreated, map[string]any{"intent_id": run.IntentID, "jira_site_id": task.JiraSiteID, "jira_issue_id": task.JiraIssueID, "issue_key": task.JiraIssueKey, "state": "queued", "version": 0, "occurred_at": now}); err != nil {
			return err
		}
		if err := s.Emit(ctx, run, "deferred_intent.promoted.v1", map[string]any{"intent_id": run.IntentID, "deferred_at": claim.ReceivedAt.Time, "promoted_at": now}); err != nil {
			return err
		}
	}
	return nil
}
