package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence/db"
)

// PgSchedulerStore is the pgx/sqlc implementation of the scheduler store
// port (VTX-5). Every admission mutation serializes on the scheduler
// advisory lock inside its own transaction — the "count held slots then
// insert the grant" pair is never split outside that lock.
type PgSchedulerStore struct {
	pool    txBeginner
	queries *db.Queries
}

func NewPgSchedulerStore(pool txBeginner, queries *db.Queries) *PgSchedulerStore {
	return &PgSchedulerStore{pool: pool, queries: queries}
}

func (s *PgSchedulerStore) GetRunForSchedule(ctx context.Context, runID string) (application.ScheduledRunView, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return application.ScheduledRunView{}, application.ErrScheduledRunNotFound
	}
	row, err := s.queries.GetRunForSchedule(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ScheduledRunView{}, application.ErrScheduledRunNotFound
	}
	if err != nil {
		return application.ScheduledRunView{}, fmt.Errorf("get run for schedule: %w", err)
	}
	return application.ScheduledRunView{
		RunID:        uuidToString(row.RunID),
		TaskID:       uuidToString(row.TaskID),
		State:        domain.State(row.State),
		JiraSiteID:   row.JiraSiteID,
		JiraIssueID:  row.JiraIssueID,
		JiraIssueKey: row.JiraIssueKey,
	}, nil
}

func (s *PgSchedulerStore) Enqueue(ctx context.Context, entry application.QueueEntryView, actor string) (application.QueueEntryView, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.QueueEntryView{}, false, fmt.Errorf("begin enqueue tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	runID, err := uuidFromString(entry.RunID)
	if err != nil {
		return application.QueueEntryView{}, false, fmt.Errorf("%w: run %s", application.ErrScheduledRunNotFound, entry.RunID)
	}
	taskID, err := uuidFromString(entry.TaskID)
	if err != nil {
		return application.QueueEntryView{}, false, fmt.Errorf("%w: task %s", application.ErrScheduledRunNotFound, entry.TaskID)
	}

	row, err := qtx.InsertQueueEntry(ctx, db.InsertQueueEntryParams{
		RunID:        runID,
		TaskID:       taskID,
		JiraSiteID:   entry.JiraSiteID,
		JiraIssueID:  entry.JiraIssueID,
		JiraIssueKey: entry.JiraIssueKey,
		PriorityRank: int32(entry.PriorityRank),
		PriorityName: textFromOptional(entry.PriorityName),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING fired: the run already has a queue row —
		// return it. Enqueue is idempotent, never a duplicate (AC-03).
		existing, gerr := qtx.GetQueueEntryByRun(ctx, runID)
		if gerr != nil {
			return application.QueueEntryView{}, false, fmt.Errorf("re-read queue entry: %w", gerr)
		}
		if err := tx.Commit(ctx); err != nil {
			return application.QueueEntryView{}, false, fmt.Errorf("commit enqueue tx: %w", err)
		}
		return queueEntryView(existing), false, nil
	}
	if err != nil {
		return application.QueueEntryView{}, false, fmt.Errorf("insert queue entry: %w", err)
	}

	if err := insertSchedEvent(ctx, qtx, "enqueued", runID, row.QueueID, pgtype.UUID{}, actor, ""); err != nil {
		return application.QueueEntryView{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return application.QueueEntryView{}, false, fmt.Errorf("commit enqueue tx: %w", err)
	}
	return queueEntryView(row), true, nil
}

func (s *PgSchedulerStore) ListSchedulable(ctx context.Context) ([]application.QueueEntryView, error) {
	rows, err := s.queries.ListSchedulableEntries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list schedulable: %w", err)
	}
	out := make([]application.QueueEntryView, 0, len(rows))
	for _, r := range rows {
		out = append(out, queueEntryView(r))
	}
	return out, nil
}

func (s *PgSchedulerStore) SetQueuePriority(ctx context.Context, queueID string, rank int, name string) error {
	id, err := uuidFromString(queueID)
	if err != nil {
		return fmt.Errorf("%w: queue %s", domain.ErrQueueNotFound, queueID)
	}
	return s.queries.SetQueuePriority(ctx, db.SetQueuePriorityParams{
		QueueID:      id,
		PriorityRank: int32(rank),
		PriorityName: textFromOptional(name),
	})
}

func (s *PgSchedulerStore) SetQueueBlocked(ctx context.Context, queueID string, status domain.QueueStatus, reason, actor string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin block tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	id, err := uuidFromString(queueID)
	if err != nil {
		return fmt.Errorf("%w: queue %s", domain.ErrQueueNotFound, queueID)
	}
	entry, err := qtx.GetQueueEntryForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: queue %s", domain.ErrQueueNotFound, queueID)
	}
	if err != nil {
		return fmt.Errorf("lock queue entry: %w", err)
	}
	// Never rewrite a terminal queue state: cancelled/released/allocated rows
	// stay as they are — a late blocked write must not resurrect them.
	if entry.Status != string(domain.QueueWaiting) &&
		entry.Status != string(domain.QueueBlockedDependency) &&
		entry.Status != string(domain.QueueBlockedUnverified) {
		return nil
	}
	if err := qtx.UpdateQueueStatus(ctx, db.UpdateQueueStatusParams{
		QueueID:       id,
		Status:        string(status),
		BlockedReason: textFromOptional(reason),
	}); err != nil {
		return fmt.Errorf("update queue status: %w", err)
	}
	if err := insertSchedEvent(ctx, qtx, "blocked", entry.RunID, id, pgtype.UUID{}, actor, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GrantPass is the atomic admission transaction: advisory lock → for each
// ordered queue entry, lock the row, confirm it is still 'waiting', confirm
// the held count still has capacity, then grant. Every check happens inside
// the same serialized transaction, so two concurrent schedulers can never
// both see capacity and both grant (AC-02).
func (s *PgSchedulerStore) GrantPass(ctx context.Context, worker string, orderedQueueIDs []string, max int, actor string) ([]application.SlotGrant, error) {
	if len(orderedQueueIDs) == 0 {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin grant tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	if err := qtx.LockScheduler(ctx); err != nil {
		return nil, fmt.Errorf("scheduler lock: %w", err)
	}

	held, err := qtx.CountHeldSlots(ctx)
	if err != nil {
		return nil, fmt.Errorf("count held slots: %w", err)
	}

	var grants []application.SlotGrant
	for _, queueIDStr := range orderedQueueIDs {
		if int(held) >= max {
			break
		}
		queueID, err := uuidFromString(queueIDStr)
		if err != nil {
			continue
		}
		entry, err := qtx.GetQueueEntryForUpdate(ctx, queueID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lock queue entry: %w", err)
		}
		// Re-check inside the lock: a cancel or a competing grant pass may
		// have moved the row since the service read it.
		if entry.Status != string(domain.QueueWaiting) {
			continue
		}

		slot, err := qtx.InsertSlotGrant(ctx, db.InsertSlotGrantParams{
			QueueID:     entry.QueueID,
			RunID:       entry.RunID,
			TaskID:      entry.TaskID,
			WorkerOwner: worker,
		})
		if err != nil {
			return nil, fmt.Errorf("insert slot grant: %w", err)
		}
		if err := qtx.MarkQueueAllocated(ctx, db.MarkQueueAllocatedParams{
			QueueID: entry.QueueID,
			GrantID: slot.GrantID,
		}); err != nil {
			return nil, fmt.Errorf("mark queue allocated: %w", err)
		}
		if err := insertSchedEvent(ctx, qtx, "granted", entry.RunID, entry.QueueID, slot.GrantID, actor, ""); err != nil {
			return nil, err
		}
		held++
		grants = append(grants, application.SlotGrant{
			GrantID:     uuidToString(slot.GrantID),
			QueueID:     uuidToString(slot.QueueID),
			RunID:       uuidToString(slot.RunID),
			TaskID:      uuidToString(slot.TaskID),
			WorkerOwner: slot.WorkerOwner,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit grant tx: %w", err)
	}
	return grants, nil
}

// ConfirmStop releases a held slot under the advisory lock. The guarded
// UPDATE makes the release exactly-once: a stale grant_id, a replay, or a
// late confirmation from a previous session matches no held row and
// therefore releases nothing — the outcome is audited either way (AC-08).
func (s *PgSchedulerStore) ConfirmStop(ctx context.Context, runID, grantID, reason, actor string) (application.ReleaseResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.ReleaseResult{}, fmt.Errorf("begin release tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	if err := qtx.LockScheduler(ctx); err != nil {
		return application.ReleaseResult{}, fmt.Errorf("scheduler lock: %w", err)
	}

	runUUID, err := uuidFromString(runID)
	if err != nil {
		return application.ReleaseResult{}, fmt.Errorf("%w: run %s", domain.ErrStaleGrant, runID)
	}
	grantUUID, err := uuidFromString(grantID)
	if err != nil {
		return application.ReleaseResult{}, fmt.Errorf("%w: grant %s", domain.ErrStaleGrant, grantID)
	}

	slot, err := qtx.ReleaseSlotGrant(ctx, db.ReleaseSlotGrantParams{
		RunID:         runUUID,
		GrantID:       grantUUID,
		ReleaseReason: textFromOptional(reason),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing held under this (run, grant): stale/duplicate/late
		// confirmation. Audited, no release.
		if err := insertSchedEvent(ctx, qtx, "released", runUUID, pgtype.UUID{}, grantUUID, actor, "ignored_stale_confirmation"); err != nil {
			return application.ReleaseResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return application.ReleaseResult{}, fmt.Errorf("commit release tx: %w", err)
		}
		return application.ReleaseResult{Released: false, RunID: runID, GrantID: grantID}, nil
	}
	if err != nil {
		return application.ReleaseResult{}, fmt.Errorf("release slot grant: %w", err)
	}

	if err := qtx.MarkQueueReleased(ctx, slot.QueueID); err != nil {
		return application.ReleaseResult{}, fmt.Errorf("mark queue released: %w", err)
	}
	if err := insertSchedEvent(ctx, qtx, "released", runUUID, slot.QueueID, grantUUID, actor, reason); err != nil {
		return application.ReleaseResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return application.ReleaseResult{}, fmt.Errorf("commit release tx: %w", err)
	}
	return application.ReleaseResult{
		Released: true,
		GrantID:  uuidToString(slot.GrantID),
		QueueID:  uuidToString(slot.QueueID),
		RunID:    uuidToString(slot.RunID),
	}, nil
}

func (s *PgSchedulerStore) RecordHeartbeat(ctx context.Context, runID, grantID string) error {
	runUUID, err := uuidFromString(runID)
	if err != nil {
		return fmt.Errorf("%w: run %s", domain.ErrStaleGrant, runID)
	}
	grantUUID, err := uuidFromString(grantID)
	if err != nil {
		return fmt.Errorf("%w: grant %s", domain.ErrStaleGrant, grantID)
	}
	_, err = s.queries.MarkHeartbeat(ctx, db.MarkHeartbeatParams{RunID: runUUID, GrantID: grantUUID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no held slot for run %s grant %s", domain.ErrStaleGrant, runID, grantID)
	}
	return err
}

func (s *PgSchedulerStore) MarkStaleSuspected(ctx context.Context, staleBefore time.Time) ([]string, error) {
	rows, err := s.queries.MarkStaleHeartbeatsSuspected(ctx, timestamptzFrom(&staleBefore))
	if err != nil {
		return nil, fmt.Errorf("mark suspected: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, uuidToString(r.RunID))
	}
	return out, nil
}

// CancelQueued atomically cancels a non-allocated entry. An allocated entry
// is the ErrSlotHeld case — the queue row is re-read to distinguish it from
// a missing one.
func (s *PgSchedulerStore) CancelQueued(ctx context.Context, runID, reason, actor string) (application.QueueEntryView, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.QueueEntryView{}, fmt.Errorf("begin cancel tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	runUUID, err := uuidFromString(runID)
	if err != nil {
		return application.QueueEntryView{}, fmt.Errorf("%w: run %s", domain.ErrQueueNotFound, runID)
	}
	entry, err := qtx.GetQueueEntryByRun(ctx, runUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.QueueEntryView{}, fmt.Errorf("%w: run %s", domain.ErrQueueNotFound, runID)
	}
	if err != nil {
		return application.QueueEntryView{}, fmt.Errorf("get queue entry: %w", err)
	}
	if entry.Status == string(domain.QueueAllocated) {
		return application.QueueEntryView{}, fmt.Errorf("%w: run %s", domain.ErrSlotHeld, runID)
	}

	cancelled, err := qtx.CancelQueueEntry(ctx, db.CancelQueueEntryParams{
		QueueID:       entry.QueueID,
		BlockedReason: textFromOptional(reason),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Raced with a grant/cancel: re-read is the honest answer.
		return application.QueueEntryView{}, fmt.Errorf("%w: run %s left the cancellable set", domain.ErrSlotHeld, runID)
	}
	if err != nil {
		return application.QueueEntryView{}, fmt.Errorf("cancel queue entry: %w", err)
	}
	if err := insertSchedEvent(ctx, qtx, "cancelled", runUUID, entry.QueueID, pgtype.UUID{}, actor, reason); err != nil {
		return application.QueueEntryView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return application.QueueEntryView{}, fmt.Errorf("commit cancel tx: %w", err)
	}
	view := queueEntryView(entry)
	view.Status = domain.QueueStatus(cancelled.Status)
	return view, nil
}

func (s *PgSchedulerStore) Metrics(ctx context.Context) (application.SchedulerMetrics, error) {
	rows, err := s.queries.SchedulerMetrics(ctx)
	if err != nil {
		return application.SchedulerMetrics{}, fmt.Errorf("metrics: %w", err)
	}
	var m application.SchedulerMetrics
	for _, r := range rows {
		switch r.Bucket {
		case "queue_waiting":
			m.Waiting = r.N
		case "queue_allocated":
			m.Allocated = r.N
		case "queue_blocked_dependency":
			m.BlockedDependency = r.N
		case "queue_blocked_unverified":
			m.BlockedUnverified = r.N
		case "queue_cancelled":
			m.Cancelled = r.N
		case "queue_released":
			m.Released = r.N
		case "slots_held":
			m.SlotsHeld = r.N
		case "slots_suspected":
			m.SlotsSuspected = r.N
		}
	}
	return m, nil
}

func insertSchedEvent(ctx context.Context, qtx *db.Queries, eventType string, runID, queueID, grantID pgtype.UUID, actor, reason string) error {
	_, err := qtx.InsertSchedulerEvent(ctx, db.InsertSchedulerEventParams{
		EventType: eventType,
		RunID:     runID,
		QueueID:   queueID,
		GrantID:   grantID,
		Actor:     actor,
		Reason:    textFromOptional(reason),
	})
	if err != nil {
		return fmt.Errorf("insert scheduler event: %w", err)
	}
	return nil
}

func queueEntryView(row db.CodingQueue) application.QueueEntryView {
	return application.QueueEntryView{
		QueueID:       uuidToString(row.QueueID),
		RunID:         uuidToString(row.RunID),
		TaskID:        uuidToString(row.TaskID),
		JiraSiteID:    row.JiraSiteID,
		JiraIssueID:   row.JiraIssueID,
		JiraIssueKey:  row.JiraIssueKey,
		PriorityRank:  int(row.PriorityRank),
		PriorityName:  textToString(row.PriorityName),
		FifoSeq:       row.FifoSeq,
		Status:        domain.QueueStatus(row.Status),
		BlockedReason: textToString(row.BlockedReason),
		GrantID:       uuidToString(row.GrantID),
		EnqueuedAt:    row.EnqueuedAt.Time,
	}
}
