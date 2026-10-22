package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/persistence/db"
)

// PgSchedulerStore is the pgx/sqlc implementation of the scheduler store
// port (VTX-5). Every slot mutation happens inside one pgx.Tx so the
// invariants — at most max_slots active grants, one grant per run/task, and
// audit matching the write — are statements about a single transaction.
type PgSchedulerStore struct {
	pool    txBeginner
	queries *db.Queries
}

func NewPgSchedulerStore(pool txBeginner, queries *db.Queries) *PgSchedulerStore {
	return &PgSchedulerStore{pool: pool, queries: queries}
}

func (s *PgSchedulerStore) EnqueueRun(ctx context.Context, runID string) (application.QueueEntry, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return application.QueueEntry{}, fmt.Errorf("%w: run %s", application.ErrEntryNotFound, runID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.QueueEntry{}, fmt.Errorf("begin enqueue tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	elig, err := qtx.GetRunSchedulingEligibility(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.QueueEntry{}, fmt.Errorf("%w: run %s", application.ErrEntryNotFound, runID)
	}
	if err != nil {
		return application.QueueEntry{}, fmt.Errorf("lock run for enqueue: %w", err)
	}
	// Only a prepared run may compete: 'preparing' + allocated worktree is
	// the evidence preparation finished (AC-09). queued/blocked/retry_wait
	// and any state without worktree evidence are rejected here — and
	// terminal runs can never enter the queue.
	if elig.State != "preparing" || !elig.HasWorktree {
		return application.QueueEntry{}, fmt.Errorf("%w: run %s state=%s worktree=%v",
			application.ErrRunNotEligible, runID, elig.State, elig.HasWorktree)
	}

	row, err := qtx.EnqueueCodingEntry(ctx, db.EnqueueCodingEntryParams{
		RunID:  id,
		TaskID: elig.TaskID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT (run_id) DO NOTHING → the entry already exists; fetch
		// it so enqueue is idempotent rather than an error.
		row, err = qtx.GetCodingQueueEntryByRun(ctx, id)
	}
	if err != nil {
		return application.QueueEntry{}, fmt.Errorf("enqueue entry: %w", err)
	}

	if err := insertSlotEvent(ctx, qtx, slotEvent{
		EntryID: row.EntryID, RunID: row.RunID,
		Action: "enqueued", Actor: "scheduler",
	}); err != nil {
		return application.QueueEntry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return application.QueueEntry{}, fmt.Errorf("commit enqueue tx: %w", err)
	}
	return queueEntryFromRow(row), nil
}

func (s *PgSchedulerStore) ListWaitingEntries(ctx context.Context, now time.Time, limit int32) ([]application.WaitingEntryView, error) {
	rows, err := s.queries.ListWaitingCodingEntries(ctx, db.ListWaitingCodingEntriesParams{
		Now:      pgtype.Timestamptz{Time: now, Valid: true},
		RowLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list waiting entries: %w", err)
	}
	views := make([]application.WaitingEntryView, 0, len(rows))
	for _, r := range rows {
		views = append(views, application.WaitingEntryView{
			QueueEntry: application.QueueEntry{
				EntryID:    uuidToString(r.EntryID),
				RunID:      uuidToString(r.RunID),
				TaskID:     uuidToString(r.TaskID),
				FifoSeq:    r.FifoSeq.Int64,
				Status:     r.Status,
				EnqueuedAt: r.EnqueuedAt.Time,
			},
			RunState:     r.RunState,
			JiraIssueID:  r.JiraIssueID,
			JiraIssueKey: r.JiraIssueKey,
		})
	}
	return views, nil
}

func (s *PgSchedulerStore) SetEntryBlock(ctx context.Context, entryID, reason string, recheckAt time.Time) error {
	id, err := uuidFromString(entryID)
	if err != nil {
		return fmt.Errorf("%w: entry %s", application.ErrEntryNotFound, entryID)
	}
	return s.queries.SetCodingEntryBlock(ctx, db.SetCodingEntryBlockParams{
		EntryID:     id,
		BlockReason: textFromOptional(reason),
		RecheckAt:   pgtype.Timestamptz{Time: recheckAt, Valid: true},
	})
}

func (s *PgSchedulerStore) GrantSlot(ctx context.Context, cmd application.GrantCommand, auditReason string) (application.GrantOutcome, error) {
	entryID, err := uuidFromString(cmd.EntryID)
	if err != nil {
		return application.GrantOutcome{}, fmt.Errorf("%w: entry %s", application.ErrEntryNotFound, cmd.EntryID)
	}
	runID, err := uuidFromString(cmd.RunID)
	if err != nil {
		return application.GrantOutcome{}, fmt.Errorf("%w: run %s", application.ErrEntryNotFound, cmd.RunID)
	}
	taskID, err := uuidFromString(cmd.TaskID)
	if err != nil {
		return application.GrantOutcome{}, fmt.Errorf("%w: task %s", application.ErrEntryNotFound, cmd.TaskID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.GrantOutcome{}, fmt.Errorf("begin grant tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	// The pool row lock serializes every grant across all scheduler replicas.
	pool, err := qtx.LockCodingSlotPool(ctx)
	if err != nil {
		return application.GrantOutcome{}, fmt.Errorf("lock slot pool: %w", err)
	}

	entry, err := qtx.GetCodingQueueEntryForUpdate(ctx, entryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.GrantOutcome{}, fmt.Errorf("%w: entry %s", application.ErrEntryNotFound, cmd.EntryID)
	}
	if err != nil {
		return application.GrantOutcome{}, fmt.Errorf("lock entry: %w", err)
	}
	if entry.Status != application.QueueStatusWaiting {
		return application.GrantOutcome{Skipped: true}, commitGrantTx(ctx, tx)
	}

	// Cancel race (AC-06): a cancel that landed between the candidate list
	// and this tx wins — locked-row read, never the stale list.
	runState, err := qtx.GetRunStateForEntry(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		runState = "cancelled"
	} else if err != nil {
		return application.GrantOutcome{}, fmt.Errorf("re-check run state: %w", err)
	}
	if runState == "completed" || runState == "failed" || runState == "cancelled" {
		if _, cerr := qtx.CloseCodingEntry(ctx, db.CloseCodingEntryParams{
			EntryID:     entryID,
			BlockReason: textFromOptional("run_terminal:" + runState),
		}); cerr != nil {
			return application.GrantOutcome{}, fmt.Errorf("close terminal entry: %w", cerr)
		}
		if err := insertSlotEvent(ctx, qtx, slotEvent{
			EntryID: entryID, RunID: runID,
			Action: "closed", Actor: cmd.Owner,
			Reason: strPtr("run_terminal:" + runState),
		}); err != nil {
			return application.GrantOutcome{}, err
		}
		return application.GrantOutcome{Closed: true}, commitGrantTx(ctx, tx)
	}

	// AC-03: an active grant for this run already exists — return it. This is
	// also the retry path after a crash between grant and caller ACK.
	if existing, err := qtx.GetActiveGrantForRun(ctx, runID); err == nil {
		return application.GrantOutcome{
			Grant:       grantFromRow(existing),
			AlreadyHeld: true,
		}, commitGrantTx(ctx, tx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return application.GrantOutcome{}, fmt.Errorf("check existing grant: %w", err)
	}

	free, err := qtx.ListFreeSlotNumbers(ctx, pool.MaxSlots)
	if err != nil {
		return application.GrantOutcome{}, fmt.Errorf("list free slots: %w", err)
	}
	if len(free) == 0 {
		return application.GrantOutcome{NoFreeSlot: true}, commitGrantTx(ctx, tx)
	}

	prior, err := qtx.CountGrantsForRun(ctx, runID)
	if err != nil {
		return application.GrantOutcome{}, fmt.Errorf("count prior grants: %w", err)
	}

	grant, err := qtx.InsertSlotGrant(ctx, db.InsertSlotGrantParams{
		SlotNo:       free[0],
		RunID:        runID,
		TaskID:       taskID,
		EntryID:      entryID,
		Owner:        cmd.Owner,
		SessionEpoch: prior + 1,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Lost a race the pool lock should have prevented — treat as
			// exhausted rather than corrupt (defence in depth).
			return application.GrantOutcome{NoFreeSlot: true}, commitGrantTx(ctx, tx)
		}
		return application.GrantOutcome{}, fmt.Errorf("insert grant: %w", err)
	}
	if err := qtx.MarkCodingEntryGranted(ctx, entryID); err != nil {
		return application.GrantOutcome{}, fmt.Errorf("mark entry granted: %w", err)
	}
	if err := insertSlotEvent(ctx, qtx, slotEvent{
		GrantID: grant.GrantID, EntryID: entryID, RunID: runID,
		Action: "granted", Actor: cmd.Owner,
		Reason: strPtr(auditReason),
	}); err != nil {
		return application.GrantOutcome{}, err
	}
	return application.GrantOutcome{Grant: grantFromRow(grant), Granted: true}, commitGrantTx(ctx, tx)
}

func commitGrantTx(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit grant tx: %w", err)
	}
	return nil
}

func (s *PgSchedulerStore) ConfirmAgentStopped(ctx context.Context, grantID string, sessionEpoch int64, reason string, at time.Time, actor string) (application.ConfirmOutcome, error) {
	id, err := uuidFromString(grantID)
	if err != nil {
		return application.ConfirmAlreadyReleased, fmt.Errorf("%w: grant %s", application.ErrGrantNotFound, grantID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.ConfirmAlreadyReleased, fmt.Errorf("begin confirm tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	grant, err := qtx.GetSlotGrantForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ConfirmAlreadyReleased, fmt.Errorf("%w: grant %s", application.ErrGrantNotFound, grantID)
	}
	if err != nil {
		return application.ConfirmAlreadyReleased, fmt.Errorf("lock grant: %w", err)
	}

	var outcome application.ConfirmOutcome
	switch {
	case grant.Status == application.GrantStatusReleased:
		outcome = application.ConfirmAlreadyReleased
	case grant.SessionEpoch != sessionEpoch:
		outcome = application.ConfirmStaleSession
	default:
		if _, err := qtx.ReleaseSlotGrant(ctx, db.ReleaseSlotGrantParams{
			GrantID:       id,
			ReleaseReason: textFromOptional(reason),
			ReleaseConfirmedAt: pgtype.Timestamptz{
				Time: at, Valid: true,
			},
			SessionEpoch: sessionEpoch,
		}); err != nil {
			return application.ConfirmAlreadyReleased, fmt.Errorf("release grant: %w", err)
		}
		outcome = application.ConfirmReleased
	}

	action := "released"
	if outcome == application.ConfirmStaleSession {
		action = "stale_confirmation"
	} else if outcome == application.ConfirmAlreadyReleased {
		action = "duplicate_confirmation"
	}
	if err := insertSlotEvent(ctx, qtx, slotEvent{
		GrantID: grant.GrantID, EntryID: grant.EntryID, RunID: grant.RunID,
		Action: action, Actor: actor,
		Reason: strPtr(reason),
	}); err != nil {
		return application.ConfirmAlreadyReleased, err
	}
	if err := tx.Commit(ctx); err != nil {
		return application.ConfirmAlreadyReleased, fmt.Errorf("commit confirm tx: %w", err)
	}
	return outcome, nil
}

func (s *PgSchedulerStore) RecordHeartbeat(ctx context.Context, grantID string, sessionEpoch int64, at time.Time) error {
	id, err := uuidFromString(grantID)
	if err != nil {
		return fmt.Errorf("%w: grant %s", application.ErrGrantNotFound, grantID)
	}
	return s.queries.RecordSlotHeartbeat(ctx, db.RecordSlotHeartbeatParams{
		GrantID:      id,
		SessionEpoch: sessionEpoch,
		HeartbeatAt:  pgtype.Timestamptz{Time: at, Valid: true},
	})
}

func (s *PgSchedulerStore) MarkSuspectGrants(ctx context.Context, cutoff time.Time, actor string) (int, error) {
	rows, err := s.queries.ListHeldGrantsStaleHeartbeat(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("list stale grants: %w", err)
	}
	for _, g := range rows {
		if err := s.queries.MarkGrantSuspect(ctx, g.GrantID); err != nil {
			return 0, fmt.Errorf("mark suspect grant %s: %w", uuidToString(g.GrantID), err)
		}
		reason := "heartbeat_lost"
		if err := insertSlotEvent(ctx, s.queries, slotEvent{
			GrantID: g.GrantID, EntryID: g.EntryID, RunID: g.RunID,
			Action: "suspect", Actor: actor, Reason: &reason,
		}); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

func (s *PgSchedulerStore) Metrics(ctx context.Context) (application.SchedulerMetrics, error) {
	row, err := s.queries.SchedulerMetrics(ctx)
	if err != nil {
		return application.SchedulerMetrics{}, fmt.Errorf("scheduler metrics: %w", err)
	}
	free := int(row.SlotsMax) - int(row.SlotsHeld) - int(row.SlotsSuspect)
	if free < 0 {
		free = 0
	}
	return application.SchedulerMetrics{
		QueueWaiting: int(row.QueueWaiting),
		QueueBlocked: int(row.QueueBlocked),
		SlotsHeld:    int(row.SlotsHeld),
		SlotsSuspect: int(row.SlotsSuspect),
		SlotsFree:    free,
		SlotsMax:     int(row.SlotsMax),
	}, nil
}

type slotEvent struct {
	GrantID pgtype.UUID
	EntryID pgtype.UUID
	RunID   pgtype.UUID
	Action  string
	Actor   string
	Reason  *string
}

func insertSlotEvent(ctx context.Context, q *db.Queries, ev slotEvent) error {
	return q.InsertCodingSlotEvent(ctx, db.InsertCodingSlotEventParams{
		GrantID:  ev.GrantID,
		EntryID:  ev.EntryID,
		RunID:    ev.RunID,
		Action:   ev.Action,
		Actor:    ev.Actor,
		Reason:   textFrom(ev.Reason),
		Metadata: nil,
	})
}

func queueEntryFromRow(row db.CodingQueueEntry) application.QueueEntry {
	e := application.QueueEntry{
		EntryID:    uuidToString(row.EntryID),
		RunID:      uuidToString(row.RunID),
		TaskID:     uuidToString(row.TaskID),
		FifoSeq:    row.FifoSeq.Int64,
		Status:     row.Status,
		EnqueuedAt: row.EnqueuedAt.Time,
	}
	if row.BlockReason.Valid {
		e.BlockReason = &row.BlockReason.String
	}
	if row.RecheckAt.Valid {
		e.RecheckAt = &row.RecheckAt.Time
	}
	return e
}

func grantFromRow(row db.CodingSlotGrant) application.SlotGrant {
	g := application.SlotGrant{
		GrantID:      uuidToString(row.GrantID),
		SlotNo:       int(row.SlotNo),
		RunID:        uuidToString(row.RunID),
		TaskID:       uuidToString(row.TaskID),
		EntryID:      uuidToString(row.EntryID),
		Owner:        row.Owner,
		SessionEpoch: row.SessionEpoch,
		Status:       row.Status,
		CreatedAt:    row.CreatedAt.Time,
	}
	if row.HeartbeatAt.Valid {
		g.HeartbeatAt = &row.HeartbeatAt.Time
	}
	if row.ReleaseReason.Valid {
		g.ReleaseReason = &row.ReleaseReason.String
	}
	if row.ReleaseConfirmedAt.Valid {
		g.ReleaseConfirmedAt = &row.ReleaseConfirmedAt.Time
	}
	return g
}

func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
