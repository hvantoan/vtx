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
	"github.com/vtx/platform/internal/requirement"
)

// OrchestratorStore is the launcher's persistence surface: requirement
// evidence (what preparing committed) plus the per-run claim ledger. It never
// writes run state directly — blocking goes through PgRequirementStore, which
// uses the registry transition path.
type OrchestratorStore struct {
	requirements *PgRequirementStore
	worktrees    *PgWorktreeStore
	q            *db.Queries
}

func NewOrchestratorStore(pool txBeginner, q *db.Queries) *OrchestratorStore {
	return &OrchestratorStore{
		requirements: NewPgRequirementStore(pool, q),
		worktrees:    NewPgWorktreeStore(pool, q),
		q:            q,
	}
}

func (s *OrchestratorStore) AllocatedRuns(ctx context.Context) ([]application.AllocatedWorktreeView, error) {
	return s.worktrees.ListAllocatedWorktrees(ctx)
}

func (s *OrchestratorStore) Input(ctx context.Context, runID string) (requirement.Input, error) {
	return s.requirements.Input(ctx, runID)
}

func (s *OrchestratorStore) Requirement(ctx context.Context, runID string) (requirement.Packet, bool, error) {
	return s.requirements.Requirement(ctx, runID)
}

func (s *OrchestratorStore) ReadBaseline(ctx context.Context, in requirement.Input) (requirement.Baseline, error) {
	return s.requirements.ReadBaseline(ctx, in)
}

func (s *OrchestratorStore) Block(ctx context.Context, in requirement.Input, code string) error {
	return s.requirements.Block(ctx, in, code)
}

// Claim takes the run over unless a live launcher holds a fresh claim. No rows
// means another instance won (or a live owner exists) — both are a silent skip.
func (s *OrchestratorStore) Claim(ctx context.Context, runID, token string, staleBefore time.Time) (bool, error) {
	uid, err := uuidFromString(runID)
	if err != nil {
		return false, err
	}
	_, err = s.q.ClaimOrchestratorRun(ctx, db.ClaimOrchestratorRunParams{
		WorkerID:    runID,
		RunID:       uid,
		ClaimToken:  token,
		StaleBefore: pgtype.Timestamptz{Time: staleBefore, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim orchestrator run: %w", err)
	}
	return true, nil
}

func (s *OrchestratorStore) Beat(ctx context.Context, runID, token string) (bool, error) {
	rows, err := s.q.BeatOrchestratorRun(ctx, db.BeatOrchestratorRunParams{WorkerID: runID, ClaimToken: token})
	if err != nil {
		return false, fmt.Errorf("beat orchestrator run: %w", err)
	}
	return rows == 1, nil
}

func (s *OrchestratorStore) Release(ctx context.Context, runID, token string) error {
	_, err := s.q.ReleaseOrchestratorRun(ctx, db.ReleaseOrchestratorRunParams{WorkerID: runID, ClaimToken: token})
	if err != nil {
		return fmt.Errorf("release orchestrator run: %w", err)
	}
	return nil
}

// GrantLease writes a fresh lease epoch for a run whose lease lapsed (for
// example after an operator resume from blocked). The caller holds the claim,
// so no second launcher can race this bump.
func (s *OrchestratorStore) GrantLease(ctx context.Context, runID, owner string, expiresAt time.Time) (int64, error) {
	uid, err := uuidFromString(runID)
	if err != nil {
		return 0, err
	}
	lease, err := s.q.AcquireRunLease(ctx, db.AcquireRunLeaseParams{
		RunID:          uid,
		LeaseOwner:     textFromOptional(owner),
		LeaseExpiresAt: pgtype.Timestamptz{Time: expiresAt.UTC(), Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("grant run lease: %w", err)
	}
	return lease.LeaseEpoch, nil
}
