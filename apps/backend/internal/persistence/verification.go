package persistence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/execcmd"
	"github.com/vtx/platform/internal/gitops"
	"github.com/vtx/platform/internal/persistence/db"
)

type PgVerificationStore struct {
	pool    txBeginner
	queries *db.Queries
}

func NewPgVerificationStore(pool txBeginner, q *db.Queries) *PgVerificationStore {
	return &PgVerificationStore{pool, q}
}
func (s *PgVerificationStore) VerificationContext(ctx context.Context, runID string) (application.RunSnapshot, string, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return application.RunSnapshot{}, "", err
	}
	run, err := s.queries.GetTaskRun(ctx, id)
	if err != nil {
		return application.RunSnapshot{}, "", fmt.Errorf("verification run: %w", err)
	}
	wt, err := s.queries.GetWorktreeByRun(ctx, id)
	if err != nil {
		return application.RunSnapshot{}, "", fmt.Errorf("verification worktree: %w", err)
	}
	if wt.Status != "allocated" {
		return application.RunSnapshot{}, "", application.ErrWorktreeNotFound
	}
	return snapshotFromTaskRun(run), wt.Path, nil
}
func (s *PgVerificationStore) NextVerificationAttempt(ctx context.Context, runID string) (int32, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return 0, err
	}
	n, err := s.queries.MaxVerificationAttempt(ctx, id)
	return n + 1, err
}
func verificationFromRow(r db.VerificationRun) application.VerificationRun {
	return application.VerificationRun{ID: uuidToString(r.VerificationID), RunID: uuidToString(r.RunID), SHA: r.SubjectCommitSha, Suite: r.Suite, Command: r.Command, Status: r.Status, Attempt: r.Attempt, StartedAt: r.StartedAt.Time, FinishedAt: r.FinishedAt.Time}
}
func (s *PgVerificationStore) BeginSuite(ctx context.Context, cmd application.VerifyCommand, r application.VerificationRun) (application.VerificationRun, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return r, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	id, err := uuidFromString(cmd.RunID)
	if err != nil {
		return r, false, err
	}
	run, err := q.GetTaskRunForUpdate(ctx, id)
	if err != nil {
		return r, false, err
	}
	if err = application.CheckRunLease(snapshotFromTaskRun(run), cmd.LeaseOwner, cmd.LeaseEpoch, time.Now()); err != nil {
		return r, false, err
	}
	if run.State != "verifying" || !run.CommitSha.Valid || run.CommitSha.String != r.SHA {
		return r, false, domain.ErrEvidenceInvalid
	}
	row, err := q.InsertVerificationRun(ctx, db.InsertVerificationRunParams{Kind: "verification", RunID: id, Attempt: r.Attempt, Suite: r.Suite, Command: r.Command, SubjectCommitSha: r.SHA, Status: "running", StartedAt: timestamptzFrom(&r.StartedAt)})
	fresh := !errors.Is(err, pgx.ErrNoRows)
	if !fresh {
		row, err = q.GetVerificationRun(ctx, db.GetVerificationRunParams{RunID: id, Attempt: r.Attempt, Suite: r.Suite})
	}
	if err != nil {
		return r, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return r, false, err
	}
	return verificationFromRow(row), fresh, nil
}
func (s *PgVerificationStore) FinishSuite(ctx context.Context, cmd application.VerifyCommand, r application.VerificationRun, green bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	id, err := uuidFromString(cmd.RunID)
	if err != nil {
		return err
	}
	run, err := q.GetTaskRunForUpdate(ctx, id)
	if err != nil {
		return err
	}
	fenceErr := application.CheckRunLease(snapshotFromTaskRun(run), cmd.LeaseOwner, cmd.LeaseEpoch, time.Now())
	if run.State != "verifying" || run.CommitSha.String != r.SHA {
		fenceErr = domain.ErrEvidenceInvalid
	}
	if fenceErr != nil {
		r.Status = "error"
		r.ErrorDetail = "lease_lost_or_sha_changed"
		green = false
	}
	vid, err := uuidFromString(r.ID)
	if err != nil {
		return err
	}
	tail := r.Result.Tail
	if len(tail) > execcmd.LogTailSize {
		tail = tail[len(tail)-execcmd.LogTailSize:]
	}
	_, err = q.FinishVerificationRun(ctx, db.FinishVerificationRunParams{VerificationID: vid, Status: r.Status, ExitCode: pgtype.Int4{Int32: int32(r.Result.ExitCode), Valid: true}, DurationMs: pgtype.Int4{Int32: int32(r.Result.Duration.Milliseconds()), Valid: true}, LogRef: textFromOptional(r.LogRef), LogTail: textFromOptional(string(tail)), LogDigest: textFromOptional(r.Result.DigestHex), ErrorDetail: textFromOptional(r.ErrorDetail), FinishedAt: timestamptzFrom(&r.FinishedAt)})
	if err != nil {
		return fmt.Errorf("finish verification: %w", err)
	}
	status := r.Status
	if green {
		status = "all_green"
	}
	metadata, err := jsonBytes(map[string]any{"status": status, "suite": r.Suite, "verification_id": r.ID, "verification_attempt": r.Attempt})
	if err != nil {
		return err
	}
	_, err = q.InsertEvidenceArtifact(ctx, db.InsertEvidenceArtifactParams{RunID: id, TaskID: run.TaskID, Attempt: run.Attempt, Kind: "test_results", Uri: r.LogRef + "#" + r.ID, Sha256: r.Result.DigestHex, CommitSha: r.SHA, Producer: "verifier", Metadata: metadata})
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return fenceErr
}
func (s *PgVerificationStore) MarkSuperseded(ctx context.Context, runID, sha string) error {
	id, err := uuidFromString(runID)
	if err != nil {
		return err
	}
	_, err = s.queries.MarkVerificationSuperseded(ctx, db.MarkVerificationSupersededParams{RunID: id, SubjectCommitSha: sha})
	return err
}

// RepinCommit moves the subject SHA to a fast-forward HEAD. It takes the same
// row lock and lease fence as BeginSuite so a lease lost between the HEAD read
// and the write cannot move the subject.
func (s *PgVerificationStore) RepinCommit(ctx context.Context, cmd application.VerifyCommand, sha string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	id, err := uuidFromString(cmd.RunID)
	if err != nil {
		return err
	}
	run, err := q.GetTaskRunForUpdate(ctx, id)
	if err != nil {
		return fmt.Errorf("repin run: %w", err)
	}
	if err := application.CheckRunLease(snapshotFromTaskRun(run), cmd.LeaseOwner, cmd.LeaseEpoch, time.Now()); err != nil {
		return err
	}
	if run.State != "verifying" {
		return domain.ErrInvalidTransition
	}
	n, err := q.RepinRunCommitSHA(ctx, db.RepinRunCommitSHAParams{RunID: id, CommitSha: sha})
	if err != nil {
		return fmt.Errorf("repin commit: %w", err)
	}
	if n != 1 {
		return domain.ErrEvidenceInvalid
	}
	return tx.Commit(ctx)
}

// FileLogSink stores bounded logs outside the run tree, under canonical roots.
type FileLogSink struct{ Root string }

func (s FileLogSink) WriteLog(ctx context.Context, r application.VerificationRun) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := filepath.Join(s.Root, r.RunID, fmt.Sprint(r.Attempt))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("log directory: %w", err)
	}
	path, err := gitops.CanonicalPath(s.Root, dir)
	if err != nil {
		return "", err
	}
	file := filepath.Join(path, r.Suite+".log")
	data := r.Result.Output
	if len(data) > execcmd.MaxCapture {
		data = data[:execcmd.MaxCapture]
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		return "", fmt.Errorf("write verification log: %w", err)
	}
	return file, nil
}
