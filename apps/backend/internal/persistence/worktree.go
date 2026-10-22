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
	"github.com/vtx/platform/internal/persistence/db"
)

// PgWorktreeStore is the pgx/sqlc implementation of the worktree store port.
// Like PgRegistryStore it owns the transaction boundary: the allocation tx is
// what makes (lease + worktree row + run branch) atomic.
type PgWorktreeStore struct {
	pool    txBeginner
	queries *db.Queries
}

func NewPgWorktreeStore(pool txBeginner, queries *db.Queries) *PgWorktreeStore {
	return &PgWorktreeStore{pool: pool, queries: queries}
}

// AllocateWorktree runs the single allocation transaction. The effect callback
// performs the on-disk work AFTER the lease is acquired and BEFORE the row is
// inserted; an error anywhere rolls back everything.
func (s *PgWorktreeStore) AllocateWorktree(
	ctx context.Context,
	cmd application.AllocateCommand,
	effect func(application.AllocateInput) (path, branch, baseSHA string, err error),
) (application.Allocation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.Allocation{}, fmt.Errorf("begin worktree tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.queries.WithTx(tx)

	runID, err := uuidFromString(cmd.RunID)
	if err != nil {
		return application.Allocation{}, fmt.Errorf("%w: run %s", application.ErrRunNotFound, cmd.RunID)
	}

	// Row lock serializes same-run allocators: the second caller blocks here,
	// then finds the committed row and returns it idempotently.
	locked, err := qtx.GetTaskRunForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.Allocation{}, fmt.Errorf("%w: run %s", application.ErrRunNotFound, cmd.RunID)
	}
	if err != nil {
		return application.Allocation{}, fmt.Errorf("lock run: %w", err)
	}

	if existing, err := qtx.GetWorktreeByRun(ctx, runID); err == nil {
		if err := tx.Commit(ctx); err != nil {
			return application.Allocation{}, fmt.Errorf("commit idempotent read: %w", err)
		}
		return application.Allocation{
			Worktree:   worktreeView(existing),
			LeaseOwner: textValue(locked.LeaseOwner),
			LeaseEpoch: locked.LeaseEpoch,
			Existing:   true,
		}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return application.Allocation{}, fmt.Errorf("check existing worktree: %w", err)
	}

	if isTerminalState(locked.State) {
		return application.Allocation{}, fmt.Errorf("%w: run %s in %s", application.ErrWorktreeRunTerminal, cmd.RunID, locked.State)
	}

	taskRepo, err := qtx.GetRunTaskRepo(ctx, runID)
	if err != nil {
		return application.Allocation{}, fmt.Errorf("read task repo: %w", err)
	}
	repoURL := textValue(taskRepo.RepoUrl)
	if repoURL == "" && cmd.RepoURL != "" {
		if err := qtx.SetRegistryTaskRepoURL(ctx, db.SetRegistryTaskRepoURLParams{
			ID:      taskRepo.TaskID,
			RepoUrl: textFromOptional(cmd.RepoURL),
		}); err != nil {
			return application.Allocation{}, fmt.Errorf("bind task repo: %w", err)
		}
		repoURL = cmd.RepoURL
	}

	lease, err := qtx.AcquireRunLease(ctx, db.AcquireRunLeaseParams{
		RunID:          runID,
		LeaseOwner:     textFromOptional(cmd.Owner),
		LeaseExpiresAt: pgtype.Timestamptz{Time: cmd.LeaseExpiresAt.UTC(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return application.Allocation{}, fmt.Errorf("%w: run %s went terminal", application.ErrWorktreeRunTerminal, cmd.RunID)
	}
	if err != nil {
		return application.Allocation{}, fmt.Errorf("acquire run lease: %w", err)
	}

	path, branch, baseSHA, err := effect(application.AllocateInput{
		RunID:        cmd.RunID,
		TaskID:       uuidToString(taskRepo.TaskID),
		State:        locked.State,
		RepoURL:      repoURL,
		JiraIssueKey: taskRepo.JiraIssueKey,
	})
	if err != nil {
		return application.Allocation{}, err
	}

	wt, err := qtx.InsertWorktree(ctx, db.InsertWorktreeParams{
		RunID:   runID,
		TaskID:  taskRepo.TaskID,
		Path:    path,
		Branch:  branch,
		RepoUrl: repoURL,
		BaseSha: textFromOptional(baseSHA),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return application.Allocation{}, fmt.Errorf("%w: task %s", application.ErrWorktreeTaskBusy, uuidToString(taskRepo.TaskID))
		}
		return application.Allocation{}, fmt.Errorf("insert worktree: %w", err)
	}
	if err := qtx.SetRunBranch(ctx, db.SetRunBranchParams{RunID: runID, Branch: textFromOptional(branch)}); err != nil {
		return application.Allocation{}, fmt.Errorf("set run branch: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return application.Allocation{}, fmt.Errorf("commit worktree tx: %w", err)
	}
	return application.Allocation{
		Worktree:   worktreeView(wt),
		LeaseOwner: textValue(lease.LeaseOwner),
		LeaseEpoch: lease.LeaseEpoch,
	}, nil
}

func (s *PgWorktreeStore) GetWorktree(ctx context.Context, runID string) (application.WorktreeView, error) {
	runUUID, err := uuidFromString(runID)
	if err != nil {
		return application.WorktreeView{}, fmt.Errorf("%w: run %s", application.ErrWorktreeNotFound, runID)
	}
	wt, err := s.queries.GetWorktreeByRun(ctx, runUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.WorktreeView{}, fmt.Errorf("%w: run %s", application.ErrWorktreeNotFound, runID)
	}
	if err != nil {
		return application.WorktreeView{}, err
	}
	return worktreeView(wt), nil
}

func (s *PgWorktreeStore) CompleteCleanup(ctx context.Context, worktreeID, fromStatus, toStatus, reason string) (application.WorktreeView, error) {
	id, err := uuidFromString(worktreeID)
	if err != nil {
		return application.WorktreeView{}, fmt.Errorf("%w: %s", application.ErrWorktreeNotFound, worktreeID)
	}
	wt, err := s.queries.MarkWorktreeStatus(ctx, db.MarkWorktreeStatusParams{
		WorktreeID:       id,
		Status:           toStatus,
		QuarantineReason: textFromOptional(reason),
		Status_2:         fromStatus,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return application.WorktreeView{}, fmt.Errorf("%w: %s no longer in %s", application.ErrWorktreeNotFound, worktreeID, fromStatus)
	}
	if err != nil {
		return application.WorktreeView{}, err
	}
	return worktreeView(wt), nil
}

func (s *PgWorktreeStore) ListAllocatedWorktrees(ctx context.Context) ([]application.AllocatedWorktreeView, error) {
	rows, err := s.queries.ListAllocatedWorktrees(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]application.AllocatedWorktreeView, 0, len(rows))
	for _, r := range rows {
		out = append(out, application.AllocatedWorktreeView{
			WorktreeView: application.WorktreeView{
				WorktreeID:       uuidToString(r.WorktreeID),
				RunID:            uuidToString(r.RunID),
				TaskID:           uuidToString(r.TaskID),
				Status:           r.Status,
				Path:             r.Path,
				Branch:           r.Branch,
				RepoURL:          r.RepoUrl,
				BaseSHA:          textPtr(r.BaseSha),
				QuarantineReason: textPtr(r.QuarantineReason),
				CreatedAt:        r.CreatedAt.Time,
				UpdatedAt:        r.UpdatedAt.Time,
				ReleasedAt:       timePtr(r.ReleasedAt),
			},
			RunState:         r.RunState,
			RunLeaseOwner:    textPtr(r.RunLeaseOwner),
			RunLeaseEpoch:    r.RunLeaseEpoch,
			RunLeaseExpireAt: timePtr(r.RunLeaseExpiresAt),
		})
	}
	return out, nil
}

func (s *PgWorktreeStore) RunLeaseState(ctx context.Context, runID string) (*string, int64, *time.Time, string, error) {
	runUUID, err := uuidFromString(runID)
	if err != nil {
		return nil, 0, nil, "", fmt.Errorf("%w: run %s", application.ErrRunNotFound, runID)
	}
	row, err := s.queries.GetRunLeaseState(ctx, runUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil, "", fmt.Errorf("%w: run %s", application.ErrRunNotFound, runID)
	}
	if err != nil {
		return nil, 0, nil, "", err
	}
	return textPtr(row.LeaseOwner), row.LeaseEpoch, timePtr(row.LeaseExpiresAt), row.State, nil
}

func (s *PgWorktreeStore) RenewRunLease(ctx context.Context, runID, owner string, epoch int64, expiresAt time.Time) (time.Time, error) {
	runUUID, err := uuidFromString(runID)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: run %s", application.ErrWorktreeLeaseLost, runID)
	}
	exp, err := s.queries.RenewRunLease(ctx, db.RenewRunLeaseParams{
		RunID:          runUUID,
		LeaseOwner:     textFromOptional(owner),
		LeaseExpiresAt: pgtype.Timestamptz{Time: expiresAt.UTC(), Valid: true},
		LeaseEpoch:     epoch,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("%w: run %s epoch %d", application.ErrWorktreeLeaseLost, runID, epoch)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("renew run lease: %w", err)
	}
	return exp.Time, nil
}

// ReleaseRunLease clears the run's lease once worktree cleanup finished.
// Owner+epoch match is enforced in SQL, so a stale owner can never clear a
// fresher lease; 0 rows is a no-op (idempotent).
func (s *PgWorktreeStore) ReleaseRunLease(ctx context.Context, runID, owner string, epoch int64) error {
	runUUID, err := uuidFromString(runID)
	if err != nil {
		return fmt.Errorf("release run lease: %w", err)
	}
	return s.queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		RunID:      runUUID,
		LeaseOwner: textFromOptional(owner),
		LeaseEpoch: epoch,
	})
}

func isTerminalState(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled"
}

func worktreeView(wt db.Worktree) application.WorktreeView {
	return application.WorktreeView{
		WorktreeID:       uuidToString(wt.WorktreeID),
		RunID:            uuidToString(wt.RunID),
		TaskID:           uuidToString(wt.TaskID),
		Status:           wt.Status,
		Path:             wt.Path,
		Branch:           wt.Branch,
		RepoURL:          wt.RepoUrl,
		BaseSHA:          textPtr(wt.BaseSha),
		QuarantineReason: textPtr(wt.QuarantineReason),
		CreatedAt:        wt.CreatedAt.Time,
		UpdatedAt:        wt.UpdatedAt.Time,
		ReleasedAt:       timePtr(wt.ReleasedAt),
	}
}

func textValue(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
