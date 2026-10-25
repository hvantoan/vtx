package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence/db"
)

type PgOperatorStore struct {
	pool    txBeginner
	queries *db.Queries
}

func NewPgOperatorStore(pool txBeginner, queries *db.Queries) *PgOperatorStore {
	return &PgOperatorStore{pool: pool, queries: queries}
}

func (s *PgOperatorStore) RunStateHistogram(ctx context.Context) (map[domain.State]int64, error) {
	rows, err := s.queries.RunStateHistogram(ctx)
	if err != nil {
		return nil, fmt.Errorf("run state histogram: %w", err)
	}
	out := make(map[domain.State]int64, len(rows))
	for _, r := range rows {
		out[domain.State(r.State)] = r.RunCount
	}
	return out, nil
}
func (s *PgOperatorStore) CountDeferredIntents(ctx context.Context) (int64, error) {
	n, e := s.queries.CountDeferredIntents(ctx)
	if e != nil {
		return 0, fmt.Errorf("count deferred intents: %w", e)
	}
	return n, nil
}
func (s *PgOperatorStore) CountActiveRuns(ctx context.Context) (int64, error) {
	n, e := s.queries.CountActiveRuns(ctx)
	if e != nil {
		return 0, fmt.Errorf("count active runs: %w", e)
	}
	return n, nil
}
func (s *PgOperatorStore) ListRuns(ctx context.Context, f application.RunFilter) ([]application.OperatorRunView, error) {
	var beforeID pgtype.UUID
	if f.BeforeRunID != nil {
		id, e := uuidFromString(*f.BeforeRunID)
		if e != nil {
			return nil, application.ErrReadNotFound
		}
		beforeID = id
	}
	var state *string
	if f.State != nil {
		v := string(*f.State)
		state = &v
	}
	rows, e := s.queries.ListRunsFiltered(ctx, db.ListRunsFilteredParams{State: textFrom(state), IssueKey: textFrom(f.IssueKey), BeforeUpdatedAt: timestamptzFrom(f.BeforeUpdatedAt), BeforeRunID: beforeID, RowLimit: f.Limit})
	if e != nil {
		return nil, fmt.Errorf("list operator runs: %w", e)
	}
	out := make([]application.OperatorRunView, 0, len(rows))
	for _, r := range rows {
		out = append(out, operatorRunView(r.RunID, r.JiraIssueKey, r.State, r.Attempt, r.CommitSha, r.PrUrl, r.LeaseOwner, r.ErrorCode, r.CreatedAt, r.UpdatedAt))
	}
	return out, nil
}
func (s *PgOperatorStore) GetRunDetail(ctx context.Context, runID string) (application.RunDetailView, error) {
	id, e := uuidFromString(runID)
	if e != nil {
		return application.RunDetailView{}, application.ErrReadNotFound
	}
	r, e := s.queries.GetOperatorRunDetail(ctx, id)
	if errors.Is(e, pgx.ErrNoRows) {
		return application.RunDetailView{}, application.ErrReadNotFound
	}
	if e != nil {
		return application.RunDetailView{}, fmt.Errorf("get operator run: %w", e)
	}
	d := application.RunDetailView{Run: operatorRunView(r.RunID, r.JiraIssueKey, r.State, r.Attempt, r.CommitSha, r.PrUrl, r.LeaseOwner, r.ErrorCode, r.CreatedAt, r.UpdatedAt)}
	if r.WorktreePath.Valid {
		d.Worktree = &application.WorktreeDetail{Path: r.WorktreePath.String, Branch: textToString(r.WorktreeBranch), BaseSHA: textPointer(r.BaseSha), Status: textToString(r.WorktreeStatus)}
	}
	if r.ErrorCode.Valid {
		d.LastError = &application.RunErrorView{ErrorCode: r.ErrorCode.String, ErrorMessage: textToString(r.ErrorMessage)}
	}
	pr, e := s.queries.GetPullRequestByRun(ctx, id)
	if e == nil {
		v := pullRequestView(pr)
		d.PullRequest = &v
	} else if !errors.Is(e, pgx.ErrNoRows) {
		return d, fmt.Errorf("get detail PR: %w", e)
	}
	if r.CommitSha.Valid {
		gate, e := s.queries.GetGateVerdict(ctx, db.GetGateVerdictParams{RunID: id, SubjectCommitSha: r.CommitSha.String})
		if e == nil {
			v := application.GateVerdict{DecisionID: uuidToString(gate.DecisionID), RunID: runID, CommitSHA: gate.SubjectCommitSha, Decision: gate.Decision, PolicyName: gate.PolicyName, PolicyVersion: gate.PolicyVersion, Evidence: gate.Conditions, EvaluatedAt: gate.EvaluatedAt.Time}
			d.GateDecision = &v
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return d, fmt.Errorf("get detail gate: %w", e)
		}
	}
	events, e := s.queries.ListTaskRunEvents(ctx, db.ListTaskRunEventsParams{RunID: id, RowLimit: 200})
	if e != nil {
		return d, fmt.Errorf("get detail timeline: %w", e)
	}
	for _, ev := range events {
		d.Timeline = append(d.Timeline, application.EventView{PreviousState: domain.State(ev.PreviousState), CurrentState: domain.State(ev.CurrentState), Action: domain.Action(ev.Action), Actor: domain.Actor(ev.Actor), ActorType: domain.ActorType(ev.ActorType), Version: ev.Version, Metadata: ev.Metadata, CreatedAt: ev.CreatedAt.Time})
	}
	return d, nil
}
func (s *PgOperatorStore) OutboxStats(ctx context.Context) ([]application.OutboxTypeStat, error) {
	rows, e := s.queries.OutboxStatsByType(ctx)
	if e != nil {
		return nil, fmt.Errorf("outbox stats: %w", e)
	}
	out := make([]application.OutboxTypeStat, 0, len(rows))
	for _, r := range rows {
		out = append(out, application.OutboxTypeStat{EventType: r.EventType, Pending: r.Pending, Retrying: r.Retrying, DeadLettered: r.DeadLettered, OldestAgeSeconds: r.OldestAgeSeconds})
	}
	return out, nil
}
func (s *PgOperatorStore) ListDeadLettered(ctx context.Context, after *string, limit int32) ([]application.DlqItem, error) {
	var cursor pgtype.UUID
	if after != nil {
		id, e := uuidFromString(*after)
		if e != nil {
			return nil, application.ErrReadNotFound
		}
		cursor = id
	}
	rows, e := s.queries.ListDeadLettered(ctx, db.ListDeadLetteredParams{After: cursor, RowLimit: limit})
	if e != nil {
		return nil, fmt.Errorf("list DLQ: %w", e)
	}
	out := make([]application.DlqItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, application.DlqItem{OutboxID: uuidToString(r.OutboxID), EventType: r.EventType, DeadLetterReason: textPointer(r.DeadLetterReason), Attempt: r.Attempt, DeadLetteredAt: r.DeadLetteredAt.Time, RedriveCount: r.RedriveCount})
	}
	return out, nil
}
func (s *PgOperatorStore) ListWorkerHeartbeats(ctx context.Context) ([]application.WorkerHeartbeatView, error) {
	rows, e := s.queries.ListWorkerHeartbeats(ctx)
	if e != nil {
		return nil, fmt.Errorf("list worker heartbeats: %w", e)
	}
	out := make([]application.WorkerHeartbeatView, 0, len(rows))
	for _, r := range rows {
		out = append(out, application.WorkerHeartbeatView{WorkerID: r.WorkerID, WorkerKind: r.WorkerKind, Queue: textPointer(r.Queue), StartedAt: r.StartedAt.Time, LastBeatAt: r.LastBeatAt.Time, ActiveRuns: r.ActiveRuns, Metadata: r.Metadata})
	}
	return out, nil
}
func (s *PgOperatorStore) RedriveDeadLetter(ctx context.Context, outboxID string, a application.OperatorAuditRecord) error {
	id, e := uuidFromString(outboxID)
	if e != nil {
		return application.ErrNotDeadLettered
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return fmt.Errorf("begin redrive tx: %w", e)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	n, e := q.RedriveDeadLetter(ctx, id)
	if e != nil {
		return fmt.Errorf("redrive dead letter: %w", e)
	}
	if n == 0 {
		a.Outcome = "rejected"
	} else {
		a.Outcome = "applied"
	}
	_, e = q.InsertOperatorAudit(ctx, db.InsertOperatorAuditParams{Actor: a.Actor, OperatorID: textFromOptional(a.OperatorID), Action: a.Action, TargetType: a.TargetType, TargetID: a.TargetID, Reason: textFromOptional(a.Reason), CorrelationID: textFromOptional(a.CorrelationID), RequestID: textFromOptional(a.RequestID), Outcome: a.Outcome, Detail: a.Detail})
	if e != nil {
		return fmt.Errorf("insert operator audit: %w", e)
	}
	if e = tx.Commit(ctx); e != nil {
		return fmt.Errorf("commit redrive tx: %w", e)
	}
	if n == 0 {
		return application.ErrNotDeadLettered
	}
	return nil
}

func operatorRunView(id pgtype.UUID, issue, state string, attempt int32, sha, url, owner, code pgtype.Text, created, updated pgtype.Timestamptz) application.OperatorRunView {
	return application.OperatorRunView{RunID: uuidToString(id), IssueKey: issue, State: domain.State(state), Attempt: attempt, CommitSHA: textPointer(sha), PRURL: textPointer(url), LeaseOwner: textPointer(owner), ErrorCode: textPointer(code), CreatedAt: created.Time, UpdatedAt: updated.Time}
}
func textPointer(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	v := t.String
	return &v
}
