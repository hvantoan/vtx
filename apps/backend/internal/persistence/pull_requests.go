package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence/db"
)

type PgPRStore struct {
	pool    txBeginner
	queries *db.Queries
}

func NewPgPRStore(pool txBeginner, queries *db.Queries) *PgPRStore {
	return &PgPRStore{pool: pool, queries: queries}
}

func (s *PgPRStore) LoadPRRun(ctx context.Context, runID string) (application.PRRun, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return application.PRRun{}, application.ErrReadNotFound
	}
	row, err := s.queries.GetPRRunContext(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.PRRun{}, application.ErrReadNotFound
	}
	if err != nil {
		return application.PRRun{}, fmt.Errorf("get PR run context: %w", err)
	}
	snapshot := application.RunSnapshot{
		RunID: uuidToString(row.RunID), TaskID: uuidToString(row.TaskID), IntentID: uuidToString(row.IntentID),
		State: domain.State(row.State), Version: row.Version, Attempt: row.Attempt,
		LeaseEpoch: row.LeaseEpoch, CorrelationID: textToString(row.CorrelationID),
	}
	if row.CommitSha.Valid {
		snapshot.CommitSHA = &row.CommitSha.String
	}
	if row.LeaseOwner.Valid {
		snapshot.LeaseOwner = &row.LeaseOwner.String
	}
	if row.LeaseExpiresAt.Valid {
		snapshot.LeaseExpiresAt = &row.LeaseExpiresAt.Time
	}
	return application.PRRun{RunSnapshot: snapshot, RepoURL: textToString(row.RepoUrl), Branch: textToString(row.Branch), IssueKey: row.JiraIssueKey}, nil
}

func (s *PgPRStore) GetGateVerdict(ctx context.Context, runID, sha string) (application.GateVerdict, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return application.GateVerdict{}, application.ErrGateEvidenceMissing
	}
	row, err := s.queries.GetGateVerdict(ctx, db.GetGateVerdictParams{RunID: id, SubjectCommitSha: sha})
	if errors.Is(err, pgx.ErrNoRows) {
		return application.GateVerdict{}, application.ErrGateEvidenceMissing
	}
	if err != nil {
		return application.GateVerdict{}, fmt.Errorf("get gate verdict: %w", err)
	}
	return application.GateVerdict{
		DecisionID: uuidToString(row.DecisionID), RunID: uuidToString(row.RunID), CommitSHA: row.SubjectCommitSha,
		Decision: row.Decision, PolicyName: row.PolicyName, PolicyVersion: row.PolicyVersion,
		Evidence: row.Conditions, EvaluatedAt: row.EvaluatedAt.Time,
	}, nil
}

func (s *PgPRStore) GetPullRequestByRun(ctx context.Context, runID string) (application.PullRequestView, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return application.PullRequestView{}, application.ErrPRNotFound
	}
	row, err := s.queries.GetPullRequestByRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.PullRequestView{}, application.ErrPRNotFound
	}
	if err != nil {
		return application.PullRequestView{}, fmt.Errorf("get pull request: %w", err)
	}
	return pullRequestView(row), nil
}

func (s *PgPRStore) SaveOpenedPullRequest(ctx context.Context, in application.SavePullRequest) (application.PullRequestView, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.PullRequestView{}, fmt.Errorf("begin pull request tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	runID, err := uuidFromString(in.RunID)
	if err != nil {
		return application.PullRequestView{}, err
	}
	intentID, err := uuidFromString(in.IntentID)
	if err != nil {
		return application.PullRequestView{}, err
	}
	opened := in.PR.CreatedAt
	if opened.IsZero() {
		opened = time.Now().UTC()
	}
	row, err := q.InsertPullRequest(ctx, db.InsertPullRequestParams{
		RunID: runID, RepoOwner: in.RepoOwner, RepoName: in.RepoName,
		PrNumber: pgtype.Int4{Int32: int32(in.PR.Number), Valid: in.PR.Number != 0},
		PrUrl:    textFromOptional(in.PR.URL), HeadBranch: in.HeadBranch, HeadSha: in.HeadSHA,
		BaseBranch: in.BaseBranch, Status: "open", GithubPrID: pgtype.Int8{Int64: in.PR.ID, Valid: in.PR.ID != 0},
		OpenedAt: pgtype.Timestamptz{Time: opened, Valid: true},
	})
	if err != nil {
		return application.PullRequestView{}, fmt.Errorf("insert pull request: %w", err)
	}
	if err := q.SetRunPrURL(ctx, db.SetRunPrURLParams{RunID: runID, PrUrl: textFromOptional(in.PR.URL)}); err != nil {
		return application.PullRequestView{}, fmt.Errorf("set run PR URL: %w", err)
	}
	eventID := uuid.New()
	payload, err := json.Marshal(map[string]any{
		"event_id": eventID.String(), "run_id": in.RunID, "task_id": in.TaskID, "intent_id": in.IntentID,
		"pr_number": in.PR.Number, "pr_url": in.PR.URL, "repo": in.RepoOwner + "/" + in.RepoName,
		"head_sha": in.HeadSHA, "base_branch": in.BaseBranch, "draft": true,
		"correlation_id": in.CorrelationID, "occurred_at": opened.Format(time.RFC3339),
	})
	if err != nil {
		return application.PullRequestView{}, fmt.Errorf("marshal pull request event: %w", err)
	}
	var outboxID pgtype.UUID
	if err := outboxID.Scan(eventID.String()); err != nil {
		return application.PullRequestView{}, err
	}
	if err := q.InsertPullRequestOpenedEvent(ctx, db.InsertPullRequestOpenedEventParams{OutboxID: outboxID, IntentID: intentID, Payload: payload}); err != nil {
		return application.PullRequestView{}, fmt.Errorf("insert pull request event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return application.PullRequestView{}, fmt.Errorf("commit pull request tx: %w", err)
	}
	return pullRequestView(row), nil
}

func (s *PgPRStore) UpdatePullRequestSync(ctx context.Context, runID, status, ciStatus string) (application.PullRequestView, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return application.PullRequestView{}, application.ErrPRNotFound
	}
	row, err := s.queries.UpdatePullRequestSync(ctx, db.UpdatePullRequestSyncParams{RunID: id, Status: status, CiStatus: textFromOptional(ciStatus)})
	if errors.Is(err, pgx.ErrNoRows) {
		return application.PullRequestView{}, application.ErrPRNotFound
	}
	if err != nil {
		return application.PullRequestView{}, fmt.Errorf("update PR sync: %w", err)
	}
	return pullRequestView(row), nil
}

func (s *PgPRStore) ListPullRequests(ctx context.Context, status *string, before *time.Time, limit int32) ([]application.PullRequestView, error) {
	rows, err := s.queries.ListPullRequestsByStatus(ctx, db.ListPullRequestsByStatusParams{Status: textFrom(status), Before: timestamptzFrom(before), RowLimit: limit})
	if err != nil {
		return nil, fmt.Errorf("list pull requests: %w", err)
	}
	out := make([]application.PullRequestView, 0, len(rows))
	for _, row := range rows {
		out = append(out, pullRequestView(row))
	}
	return out, nil
}

func pullRequestView(row db.PullRequest) application.PullRequestView {
	v := application.PullRequestView{PullRequestID: uuidToString(row.PrID), RunID: uuidToString(row.RunID), RepoOwner: row.RepoOwner, RepoName: row.RepoName, Draft: row.Draft, HeadBranch: row.HeadBranch, HeadSHA: row.HeadSha, BaseBranch: row.BaseBranch, Status: row.Status, CreatedAt: row.CreatedAt.Time}
	if row.PrNumber.Valid {
		n := int(row.PrNumber.Int32)
		v.Number = &n
	}
	if row.PrUrl.Valid {
		v.URL = &row.PrUrl.String
	}
	if row.CiStatus.Valid {
		v.CIStatus = &row.CiStatus.String
	}
	if row.LastSyncedAt.Valid {
		v.LastSyncedAt = &row.LastSyncedAt.Time
	}
	if row.ErrorCode.Valid {
		v.ErrorCode = &row.ErrorCode.String
	}
	return v
}
