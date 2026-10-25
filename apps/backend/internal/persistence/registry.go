package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence/db"
)

// PgRegistryStore is the pgx/sqlc implementation of the registry store port.
// It owns the transaction boundaries the registry's invariants are stated in
// terms of (I-07): every mutation of task, run, audit, receipt and outbox for
// one logical step commits or rolls back together.
// txBeginner is the slice of *pgxpool.Pool this adapter needs.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type PgRegistryStore struct {
	pool    txBeginner
	queries *db.Queries
	guards  map[TransitionKey]TransitionGuard
}

func NewPgRegistryStore(pool txBeginner, queries *db.Queries) *PgRegistryStore {
	return &PgRegistryStore{pool: pool, queries: queries}
}

// uniqueViolationActiveRun is the partial unique index implementing I-03.
const uniqueViolation = "23505"

func (s *PgRegistryStore) TransitionRun(
	ctx context.Context,
	cmd application.TransitionCommand,
	decide func(application.RunSnapshot) (application.TransitionPlan, error),
) (application.RunSnapshot, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.RunSnapshot{}, fmt.Errorf("begin registry tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	runID, err := uuidFromString(cmd.RunID)
	if err != nil {
		return application.RunSnapshot{}, fmt.Errorf("%w: run %s", application.ErrRunNotFound, cmd.RunID)
	}

	// The row lock is what makes the §0.D error classification deterministic:
	// whoever commits first advances the version, everyone else blocks here and
	// then reads the new version (TR-07).
	locked, err := qtx.GetTaskRunForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.RunSnapshot{}, fmt.Errorf("%w: run %s", application.ErrRunNotFound, cmd.RunID)
	}
	if err != nil {
		return application.RunSnapshot{}, fmt.Errorf("lock run: %w", err)
	}

	plan, err := decide(snapshotFromTaskRun(locked))
	if err != nil {
		return application.RunSnapshot{}, err
	}

	if guard := s.guards[TransitionKey{domain.State(locked.State), cmd.Action}]; guard != nil {
		if err := guard.Check(ctx, qtx, locked, cmd, &plan); err != nil {
			return application.RunSnapshot{}, err
		}
	}

	updated, err := qtx.UpdateTaskRunState(ctx, db.UpdateTaskRunStateParams{
		RunID:        locked.RunID,
		State:        string(plan.Update.State),
		ResumeState:  textFromState(plan.Update.ResumeState),
		RetryDueAt:   timestamptzFrom(plan.Update.RetryDueAt),
		Attempt:      plan.Update.Attempt,
		ErrorCode:    textFrom(plan.Update.ErrorCode),
		ErrorMessage: textFrom(plan.Update.ErrorMessage),
		StartedAt:    timestamptzFrom(plan.Update.StartedAt),
		FinishedAt:   timestamptzFrom(plan.Update.FinishedAt),
		Version:      locked.Version,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Unreachable while the row stays locked; reported as the domain error
		// it means rather than a generic failure.
		return application.RunSnapshot{}, fmt.Errorf("%w: run %s", domain.ErrStaleVersion, cmd.RunID)
	}
	if err != nil {
		return application.RunSnapshot{}, fmt.Errorf("update run: %w", err)
	}

	if _, err := qtx.InsertTaskRunEvent(ctx, db.InsertTaskRunEventParams{
		RunID:         locked.RunID,
		PreviousState: string(plan.Audit.PreviousState),
		CurrentState:  string(plan.Audit.CurrentState),
		Action:        string(plan.Audit.Action),
		Actor:         string(plan.Audit.Actor),
		ActorType:     string(plan.Audit.ActorType),
		CorrelationID: textFromOptional(plan.Audit.CorrelationID),
		Version:       plan.Audit.Version,
		Metadata:      plan.Audit.Metadata,
	}); err != nil {
		return application.RunSnapshot{}, fmt.Errorf("insert run audit: %w", err)
	}

	if _, err := qtx.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
		IntentID:  locked.IntentID, // NOT NULL in VTX-2: every registry event carries it
		EventType: plan.Outbound.EventType,
		Payload:   plan.Outbound.Payload,
	}); err != nil {
		return application.RunSnapshot{}, fmt.Errorf("insert outbound event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return application.RunSnapshot{}, fmt.Errorf("commit registry tx: %w", err)
	}
	return snapshotFromTaskRun(updated), nil
}

func (s *PgRegistryStore) ConsumeIntent(
	ctx context.Context,
	plan application.ConsumePlan,
	evt application.IntentEvent,
) (application.ConsumeOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("begin intake tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	eventID, err := uuidFromString(evt.EventID)
	if err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("%w: event_id %q", application.ErrPayloadInvalid, evt.EventID)
	}
	intentID, err := uuidFromString(evt.IntentID)
	if err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("%w: intent_id %q", application.ErrPayloadInvalid, evt.IntentID)
	}

	// Serialize every delivery for the same intent before using the
	// source-event receipt as the transport-level dedup gate. This prevents a
	// different source_event_id from turning an already admitted intent into a
	// deferred intent (or emitting another deferred outbox event).
	if _, err := qtx.LockExecutionIntent(ctx, intentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ConsumeOutcome{}, fmt.Errorf("%w: intent %s has no execution_intents row", application.ErrPayloadInvalid, evt.IntentID)
		}
		return application.ConsumeOutcome{}, fmt.Errorf("lock execution intent: %w", err)
	}

	// Once the intent lock is held, the receipt is the transport-level dedup
	// gate: a replay finds the row and leaves the database untouched (I-02,
	// TR-19).
	if _, err := qtx.InsertInboxConsumption(ctx, db.InsertInboxConsumptionParams{
		ConsumerName:  plan.Consumer,
		SourceEventID: eventID,
	}); errors.Is(err, pgx.ErrNoRows) {
		return application.ConsumeOutcome{Duplicate: true}, nil
	} else if err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("insert receipt: %w", err)
	}

	consumed, err := qtx.HasConsumedIntent(ctx, intentID)
	if err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("check consumed intent: %w", err)
	}
	if consumed {
		if err := tx.Commit(ctx); err != nil {
			return application.ConsumeOutcome{}, fmt.Errorf("commit duplicate intent receipt: %w", err)
		}
		return application.ConsumeOutcome{Duplicate: true}, nil
	}

	task, err := qtx.UpsertRegistryTask(ctx, db.UpsertRegistryTaskParams{
		JiraSiteID:   plan.Task.JiraSiteID,
		JiraIssueID:  plan.Task.JiraIssueID,
		JiraIssueKey: plan.Task.JiraIssueKey,
		ProjectKey:   plan.Task.ProjectKey,
		// Intake does not carry a repo (VTX-2 payload); NULL keeps any value a
		// previous allocation recorded via the COALESCE in the upsert.
		RepoUrl: pgtype.Text{},
	})
	if err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("upsert task: %w", err)
	}
	outcome := application.ConsumeOutcome{TaskID: uuidToString(task.ID)}

	active, err := qtx.HasActiveRunForTask(ctx, task.ID)
	if err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("check active run: %w", err)
	}

	var branch application.ConsumeBranch
	var run db.TaskRun
	admitted := false

	if !active {
		run, err = qtx.InsertTaskRun(ctx, db.InsertTaskRunParams{
			TaskID:        task.ID,
			IntentID:      intentID,
			CorrelationID: textFromOptional(evt.CorrelationID),
			OriginEventID: eventID,
			ChangelogID:   textFromOptional(evt.ChangelogID),
		})
		switch {
		case err == nil:
			admitted = true
			branch = plan.OnAdmit
		case isUniqueViolation(err):
			// Raced with another dispatcher replica between the check and the
			// insert: the durable wait queue is the correct landing place, not
			// an error (I-04).
			admitted = false
		case isForeignKeyViolation(err):
			// The intent row is written in the same transaction as the outbox
			// event we were handed, so a missing intent is a contract fault, not
			// a race: quarantine it instead of retrying forever.
			return application.ConsumeOutcome{}, fmt.Errorf("%w: intent %s has no execution_intents row", application.ErrPayloadInvalid, evt.IntentID)
		default:
			return application.ConsumeOutcome{}, fmt.Errorf("insert run: %w", err)
		}
	}

	if !admitted {
		branch = plan.OnDefer
		if _, err := qtx.InsertDeferredIntent(ctx, db.InsertDeferredIntentParams{
			IntentID:    intentID,
			TaskID:      task.ID,
			Disposition: "pending_deferred",
			Reason:      textFromOptional(branch.Reason),
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return application.ConsumeOutcome{}, fmt.Errorf("insert deferred intent: %w", err)
		}
		outcome.Deferred = true
	} else if err := qtx.LinkInboxConsumptionToRun(ctx, db.LinkInboxConsumptionToRunParams{
		ConsumerName:  plan.Consumer,
		SourceEventID: eventID,
		RunID:         run.RunID,
	}); err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("link receipt to run: %w", err)
	}

	// The task identity is known on both branches (defer has no run yet).
	snapshot := application.RunSnapshot{TaskID: uuidToString(task.ID)}
	if admitted {
		snapshot = snapshotFromTaskRun(run)
		outcome.RunID = snapshot.RunID
	}

	if branch.Audit != nil && admitted {
		if _, err := qtx.InsertTaskRunEvent(ctx, db.InsertTaskRunEventParams{
			RunID:         run.RunID,
			PreviousState: string(branch.Audit.PreviousState),
			CurrentState:  string(branch.Audit.CurrentState),
			Action:        string(branch.Audit.Action),
			Actor:         string(branch.Audit.Actor),
			ActorType:     string(branch.Audit.ActorType),
			CorrelationID: textFromOptional(branch.Audit.CorrelationID),
			Version:       branch.Audit.Version,
			Metadata:      branch.Audit.Metadata,
		}); err != nil {
			return application.ConsumeOutcome{}, fmt.Errorf("insert creation audit: %w", err)
		}
	}

	outbound, err := branch.Outbound(snapshot)
	if err != nil {
		return application.ConsumeOutcome{}, err
	}
	outcome.OutboundType = outbound.EventType
	if _, err := qtx.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
		IntentID:  intentID,
		EventType: outbound.EventType,
		Payload:   outbound.Payload,
	}); err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("insert outbound event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return application.ConsumeOutcome{}, fmt.Errorf("commit intake tx: %w", err)
	}
	return outcome, nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation. Two constraints can fire here: task_runs.intent_id (a run for this
// intent already exists) and the I-03 partial index (another run is active).
// Both mean "do not create a second run".
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

// isForeignKeyViolation reports a broken reference (23503): the payload points
// at a row that does not exist.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func snapshotFromTaskRun(row db.TaskRun) application.RunSnapshot {
	snapshot := application.RunSnapshot{
		RunID:         uuidToString(row.RunID),
		TaskID:        uuidToString(row.TaskID),
		IntentID:      uuidToString(row.IntentID),
		State:         domain.State(row.State),
		Version:       row.Version,
		Attempt:       row.Attempt,
		LeaseEpoch:    row.LeaseEpoch,
		CorrelationID: textToString(row.CorrelationID),
	}
	if state := domain.State(textToString(row.ResumeState)); state.Valid() {
		snapshot.ResumeState = &state
	}
	if row.RetryDueAt.Valid {
		snapshot.RetryDueAt = &row.RetryDueAt.Time
	}
	if row.LeaseOwner.Valid {
		snapshot.LeaseOwner = &row.LeaseOwner.String
	}
	if row.LeaseExpiresAt.Valid {
		snapshot.LeaseExpiresAt = &row.LeaseExpiresAt.Time
	}
	if row.CommitSha.Valid {
		snapshot.CommitSHA = &row.CommitSha.String
	}
	if row.StartedAt.Valid {
		snapshot.StartedAt = &row.StartedAt.Time
	}
	if row.FinishedAt.Valid {
		snapshot.FinishedAt = &row.FinishedAt.Time
	}
	return snapshot
}

func uuidFromString(value string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(value); err != nil {
		return pgtype.UUID{}, fmt.Errorf("parse uuid %q: %w", value, err)
	}
	return id, nil
}

func uuidToString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func textFrom(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}

func textFromOptional(value string) pgtype.Text {
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}

func textToString(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func textFromState(state *domain.State) pgtype.Text {
	if state == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: string(*state), Valid: true}
}

func timestamptzFrom(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *value, Valid: true}
}

// Read side (Phase 5). Separate methods, not a separate type: the adapter is
// already the one place that knows both sqlc rows and the read models.

func (s *PgRegistryStore) GetTask(ctx context.Context, siteID, issueID string) (application.TaskView, error) {
	row, err := s.queries.GetRegistryTaskByIdentity(ctx, db.GetRegistryTaskByIdentityParams{
		JiraSiteID: siteID, JiraIssueID: issueID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return application.TaskView{}, application.ErrReadNotFound
	}
	if err != nil {
		return application.TaskView{}, fmt.Errorf("get task: %w", err)
	}
	return application.TaskView{
		TaskID:       uuidToString(row.ID),
		JiraSiteID:   row.JiraSiteID,
		JiraIssueID:  row.JiraIssueID,
		JiraIssueKey: row.JiraIssueKey,
		ProjectKey:   row.ProjectKey,
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}, nil
}

func (s *PgRegistryStore) GetRun(ctx context.Context, runID string) (application.RunView, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return application.RunView{}, application.ErrReadNotFound
	}
	row, err := s.queries.GetTaskRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.RunView{}, application.ErrReadNotFound
	}
	if err != nil {
		return application.RunView{}, fmt.Errorf("get run: %w", err)
	}
	return runViewFromTaskRun(row), nil
}

func (s *PgRegistryStore) ListEvents(ctx context.Context, runID string, afterVersion *int64, limit int32) ([]application.EventView, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return nil, application.ErrReadNotFound
	}
	rows, err := s.queries.ListTaskRunEvents(ctx, db.ListTaskRunEventsParams{
		RunID:        id,
		AfterVersion: int8FromOptional(afterVersion),
		RowLimit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list run events: %w", err)
	}
	views := make([]application.EventView, 0, len(rows))
	for _, row := range rows {
		views = append(views, application.EventView{
			PreviousState: domain.State(row.PreviousState),
			CurrentState:  domain.State(row.CurrentState),
			Action:        domain.Action(row.Action),
			Actor:         domain.Actor(row.Actor),
			ActorType:     domain.ActorType(row.ActorType),
			Version:       row.Version,
			Metadata:      row.Metadata,
			CreatedAt:     row.CreatedAt.Time,
		})
	}
	return views, nil
}

func runViewFromTaskRun(row db.TaskRun) application.RunView {
	view := application.RunView{
		RunID:         uuidToString(row.RunID),
		TaskID:        uuidToString(row.TaskID),
		State:         domain.State(row.State),
		Version:       row.Version,
		Attempt:       row.Attempt,
		LeaseEpoch:    row.LeaseEpoch,
		CorrelationID: textToString(row.CorrelationID),
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
	}
	if state := domain.State(textToString(row.ResumeState)); state.Valid() {
		view.ResumeState = &state
	}
	if row.RetryDueAt.Valid {
		view.RetryDueAt = &row.RetryDueAt.Time
	}
	if row.CommitSha.Valid {
		view.CommitSHA = &row.CommitSha.String
	}
	if row.Branch.Valid {
		view.Branch = &row.Branch.String
	}
	if row.ErrorCode.Valid {
		view.ErrorCode = &row.ErrorCode.String
	}
	if row.ErrorMessage.Valid {
		view.ErrorMessage = &row.ErrorMessage.String
	}
	if row.LeaseOwner.Valid {
		view.LeaseOwner = &row.LeaseOwner.String
	}
	if row.LeaseExpiresAt.Valid {
		view.LeaseExpiresAt = &row.LeaseExpiresAt.Time
	}
	if row.StartedAt.Valid {
		view.StartedAt = &row.StartedAt.Time
	}
	if row.FinishedAt.Valid {
		view.FinishedAt = &row.FinishedAt.Time
	}
	return view
}

func int8FromOptional(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}
