package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence/db"
	"github.com/vtx/platform/internal/requirement"
)

type PgRequirementStore struct {
	pool txBeginner
	q    *db.Queries
}

func NewPgRequirementStore(pool txBeginner, q *db.Queries) *PgRequirementStore {
	return &PgRequirementStore{pool, q}
}
func (s *PgRequirementStore) Input(ctx context.Context, id string) (requirement.Input, error) {
	uid, err := uuidFromString(id)
	if err != nil {
		return requirement.Input{}, domain.ErrNotFound
	}
	r, err := s.q.GetRequirementInput(ctx, uid)
	if err != nil {
		return requirement.Input{}, fmt.Errorf("requirement input: %w", err)
	}
	run := application.RunSnapshot{RunID: id, TaskID: uuidToString(r.TaskID), IntentID: uuidToString(r.IntentID), State: domain.State(r.State), Version: r.Version, Attempt: r.Attempt, LeaseEpoch: r.LeaseEpoch, CorrelationID: textToString(r.CorrelationID)}
	if r.LeaseOwner.Valid {
		run.LeaseOwner = &r.LeaseOwner.String
	}
	if r.LeaseExpiresAt.Valid {
		run.LeaseExpiresAt = &r.LeaseExpiresAt.Time
	}
	in := requirement.Input{Run: run, SiteID: r.JiraSiteID, IssueID: r.JiraIssueID, IssueKey: r.JiraIssueKey, ProjectKey: r.ProjectKey}
	w, err := s.q.GetWorktreeByRun(ctx, uid)
	if err == nil {
		in.Worktree = worktreeView(w)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return requirement.Input{}, fmt.Errorf("requirement worktree: %w", err)
	}
	return in, nil
}
func (s *PgRequirementStore) SchedulerIssue(ctx context.Context, id string) (string, string, string, string, error) {
	uid, err := uuidFromString(id)
	if err != nil {
		return "", "", "", "", err
	}
	r, err := s.q.GetRequirementInput(ctx, uid)
	return r.JiraSiteID, r.JiraIssueID, r.ProjectKey, "", err
}
func packetFromRow(r db.RunRequirement) (requirement.Packet, error) {
	var p requirement.Packet
	p.IssueKey = r.IssueKey
	p.Summary = r.Summary
	p.RequirementVersion = r.RequirementVersion
	p.RequirementHash = r.RequirementHash
	if err := json.Unmarshal(r.AcceptanceCriteria, &p.AcceptanceCriteria); err != nil {
		return p, fmt.Errorf("decode criteria: %w", err)
	}
	if err := json.Unmarshal(r.IssueSnapshot, &p.Snapshot); err != nil {
		return p, fmt.Errorf("decode snapshot: %w", err)
	}
	p.Description = p.Snapshot.Description
	return p, nil
}
func (s *PgRequirementStore) Requirement(ctx context.Context, id string) (requirement.Packet, bool, error) {
	uid, err := uuidFromString(id)
	if err != nil {
		return requirement.Packet{}, false, err
	}
	row, err := s.q.GetRunRequirement(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return requirement.Packet{}, false, nil
	}
	if err != nil {
		return requirement.Packet{}, false, err
	}
	p, err := packetFromRow(row)
	return p, true, err
}
func validPreparation(r db.TaskRun, in requirement.Input) error {
	if r.State != "preparing" {
		return domain.ErrInvalidTransition
	}
	if r.Version != in.Run.Version {
		return domain.ErrStaleVersion
	}
	if !r.LeaseOwner.Valid || in.Run.LeaseOwner == nil || r.LeaseOwner.String != *in.Run.LeaseOwner || r.LeaseEpoch != in.Run.LeaseEpoch || !r.LeaseExpiresAt.Valid || !r.LeaseExpiresAt.Time.After(time.Now()) {
		return domain.ErrStaleLease
	}
	return nil
}
func (s *PgRequirementStore) SaveRequirement(ctx context.Context, in requirement.Input, p requirement.Packet, now time.Time) (requirement.Packet, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	id, err := uuidFromString(in.Run.RunID)
	if err != nil {
		return p, err
	}
	locked, err := q.GetTaskRunForUpdate(ctx, id)
	if err != nil {
		return p, err
	}
	if err := validPreparation(locked, in); err != nil {
		return p, err
	}
	// Retry returns the frozen winner, rather than publishing duplicate ready
	// events or silently replacing another replica's snapshot.
	existing, err := q.GetRunRequirement(ctx, id)
	if err == nil {
		return packetFromRow(existing)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	ac, err := json.Marshal(p.AcceptanceCriteria)
	if err != nil {
		return p, err
	}
	snapshot, err := json.Marshal(p.Snapshot)
	if err != nil {
		return p, err
	}
	_, err = q.UpsertRunRequirement(ctx, db.UpsertRunRequirementParams{RunID: id, IntentID: locked.IntentID, JiraSiteID: in.SiteID, IssueID: in.IssueID, IssueKey: p.IssueKey, RequirementVersion: p.RequirementVersion, RequirementHash: p.RequirementHash, Summary: p.Summary, AcceptanceCriteria: ac, IssueSnapshot: snapshot, FetchedAt: pgtype.Timestamptz{Time: now, Valid: true}})
	if err != nil {
		return p, err
	}
	if err := emitRequirement(ctx, q, locked, "run.requirement_ready.v1", map[string]any{"issue_key": p.IssueKey, "requirement_version": p.RequirementVersion, "requirement_hash": p.RequirementHash, "ac_count": len(p.AcceptanceCriteria), "fetched_at": now, "occurred_at": now}); err != nil {
		return p, err
	}
	if err := tx.Commit(ctx); err != nil {
		return p, fmt.Errorf("commit requirement: %w", err)
	}
	return p, nil
}
func emitRequirement(ctx context.Context, q *db.Queries, r db.TaskRun, kind string, body map[string]any) error {
	body["run_id"] = uuidToString(r.RunID)
	body["correlation_id"] = textToString(r.CorrelationID)
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{IntentID: r.IntentID, EventType: kind, Payload: payload})
	return err
}
func baselineFromRow(r db.VerificationRun) requirement.Baseline {
	status := r.Status
	if status != "passed" && status != "failed" {
		status = "inconclusive"
	}
	return requirement.Baseline{VerificationID: uuidToString(r.VerificationID), Suite: r.Suite, Status: status, ArtifactIDs: []string{}}
}
func (s *PgRequirementStore) ReadBaseline(ctx context.Context, in requirement.Input) (requirement.Baseline, error) {
	if in.Worktree.BaseSHA == nil {
		return requirement.Baseline{}, domain.ErrEvidenceInvalid
	}
	id, err := uuidFromString(in.Run.RunID)
	if err != nil {
		return requirement.Baseline{}, err
	}
	row, err := s.q.GetRunBaseline(ctx, db.GetRunBaselineParams{RunID: id, SubjectCommitSha: *in.Worktree.BaseSHA})
	if errors.Is(err, pgx.ErrNoRows) {
		return requirement.Baseline{}, domain.ErrEvidenceInvalid
	}
	if err != nil {
		return requirement.Baseline{}, err
	}
	return baselineFromRow(row), nil
}
func (s *PgRequirementStore) Baseline(ctx context.Context, in requirement.Input, command string, now time.Time, execute func(context.Context) (requirement.SuiteResult, error)) (requirement.Baseline, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return requirement.Baseline{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := s.q.WithTx(tx)
	id, err := uuidFromString(in.Run.RunID)
	if err != nil {
		return requirement.Baseline{}, err
	}
	if err := q.LockRunBaseline(ctx, id); err != nil {
		return requirement.Baseline{}, err
	}
	// Do not hold a run-row lock during execution: lease renewals and operator
	// cancellation must remain able to commit while a suite is running.
	run, err := q.GetTaskRun(ctx, id)
	if err != nil {
		return requirement.Baseline{}, err
	}
	if err := validPreparation(run, in); err != nil {
		return requirement.Baseline{}, err
	}
	existing, err := q.GetRunBaseline(ctx, db.GetRunBaselineParams{RunID: id, SubjectCommitSha: *in.Worktree.BaseSHA})
	if err == nil {
		return baselineFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return requirement.Baseline{}, err
	}
	row, err := q.InsertRunBaseline(ctx, db.InsertRunBaselineParams{RunID: id, Attempt: in.Run.Attempt + 1, Suite: command, Command: command, SubjectCommitSha: *in.Worktree.BaseSHA, StartedAt: pgtype.Timestamptz{Time: now, Valid: true}})
	if err != nil {
		return requirement.Baseline{}, err
	}
	result, runErr := execute(ctx)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	locked, err := q.GetTaskRunForUpdate(finishCtx, id)
	if err != nil {
		return requirement.Baseline{}, err
	}
	if err := validPreparation(locked, in); err != nil {
		return requirement.Baseline{}, err
	}
	status := "passed"
	if result.ExitCode != 0 {
		status = "failed"
	}
	if runErr != nil {
		status = "error"
	}
	if result.TimedOut {
		status = "timeout"
	}
	digest := result.Digest
	if digest == "" {
		sum := sha256.Sum256(result.Output)
		digest = hex.EncodeToString(sum[:])
	}
	tail := result.Output
	if len(tail) > 64*1024 {
		tail = tail[len(tail)-64*1024:]
	}
	row, err = q.FinishRunBaseline(finishCtx, db.FinishRunBaselineParams{VerificationID: row.VerificationID, Status: status, ExitCode: pgtype.Int4{Int32: int32(result.ExitCode), Valid: runErr == nil}, DurationMs: pgtype.Int4{Int32: int32(result.Duration.Milliseconds()), Valid: true}, LogTail: textFromOptional(string(tail)), LogDigest: textFromOptional(digest), FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}})
	if err != nil {
		return requirement.Baseline{}, err
	}
	baseline := baselineFromRow(row)
	if err := emitRequirement(finishCtx, q, locked, "run.baseline_recorded.v1", map[string]any{"base_sha": *in.Worktree.BaseSHA, "suite": command, "status": baseline.Status, "verification_run_id": baseline.VerificationID, "occurred_at": time.Now()}); err != nil {
		return requirement.Baseline{}, err
	}
	if err := tx.Commit(finishCtx); err != nil {
		return requirement.Baseline{}, fmt.Errorf("commit baseline: %w", err)
	}
	return baseline, nil
}
func (s *PgRequirementStore) Block(ctx context.Context, in requirement.Input, code string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	registry := application.NewRegistryService(NewPgRegistryStore(tx, q), "requirements", nil, nil)
	_, err = registry.TransitionRun(ctx, application.TransitionCommand{RunID: in.Run.RunID, ExpectedVersion: in.Run.Version, ExpectedLeaseEpoch: in.Run.LeaseEpoch, Action: domain.ActionBlock, Actor: domain.ActorPolicy, ActorType: domain.ActorTypeSystem, Reason: code})
	if err != nil {
		return err
	}
	id, err := uuidFromString(in.Run.RunID)
	if err != nil {
		return err
	}
	if err := q.SetSchedulerRunError(ctx, db.SetSchedulerRunErrorParams{RunID: id, ErrorCode: textFromOptional(code)}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *PgRequirementStore) Renew(ctx context.Context, in requirement.Input, expiry time.Time) error {
	if in.Run.LeaseOwner == nil {
		return domain.ErrStaleLease
	}
	id, err := uuidFromString(in.Run.RunID)
	if err != nil {
		return err
	}
	_, err = s.q.RenewRunLease(ctx, db.RenewRunLeaseParams{RunID: id, LeaseOwner: textFrom(in.Run.LeaseOwner), LeaseEpoch: in.Run.LeaseEpoch, LeaseExpiresAt: pgtype.Timestamptz{Time: expiry, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrStaleLease
	}
	return err
}
