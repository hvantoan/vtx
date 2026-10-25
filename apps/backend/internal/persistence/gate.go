package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/vtx/platform/internal/gate"
	"github.com/vtx/platform/internal/persistence/db"
)

type PgGateService struct {
	pool    txBeginner
	queries *db.Queries
	Guards  EvidenceGuards
}

func NewPgGateService(pool txBeginner, q *db.Queries, g EvidenceGuards) *PgGateService {
	return &PgGateService{pool, q, g}
}

// Evaluate locks the run and persists decision+outbox atomically. The advance
// path calls the same evaluateTx on the registry transaction, never this tx.
func (s *PgGateService) Evaluate(ctx context.Context, runID string, epoch int64) (gate.Decision, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return gate.Decision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	id, err := uuidFromString(runID)
	if err != nil {
		return gate.Decision{}, err
	}
	run, err := q.GetTaskRunForUpdate(ctx, id)
	if err != nil {
		return gate.Decision{}, err
	}
	d, err := s.Guards.evaluateTx(ctx, q, run, epoch)
	if err != nil {
		return d, err
	}
	if err = tx.Commit(ctx); err != nil {
		return d, err
	}
	return d, nil
}
func gateFromRow(r db.GateDecision) (gate.Decision, error) {
	d := gate.Decision{ID: uuidToString(r.DecisionID), RunID: uuidToString(r.RunID), Attempt: r.Attempt, SubjectCommitSHA: r.SubjectCommitSha, VerifiedHeadSHA: r.VerifiedHeadSha.String, PolicyName: r.PolicyName, PolicyVersion: r.PolicyVersion, Decision: r.Decision, BlockingReasons: r.BlockingReasons, EvaluatedAt: r.EvaluatedAt.Time}
	if err := json.Unmarshal(r.Conditions, &d.Conditions); err != nil {
		return d, fmt.Errorf("decode gate conditions: %w", err)
	}
	return d, nil
}
func (s *PgGateService) LatestDecision(ctx context.Context, runID, sha string) (*gate.Decision, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return nil, err
	}
	row, err := s.queries.LatestGateDecisionForSHA(ctx, db.LatestGateDecisionForSHAParams{RunID: id, SubjectCommitSha: sha})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := gateFromRow(row)
	return &d, err
}
func (s *PgGateService) History(ctx context.Context, runID string) ([]gate.Decision, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return nil, err
	}
	rows, err := s.queries.ListGateDecisions(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]gate.Decision, 0, len(rows))
	for _, row := range rows {
		d, err := gateFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}
