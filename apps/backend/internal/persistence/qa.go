package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence/db"
	"github.com/vtx/platform/internal/qa"
)

type PgQAStore struct {
	pool    txBeginner
	queries *db.Queries
}

func NewPgQAStore(pool txBeginner, q *db.Queries) *PgQAStore { return &PgQAStore{pool, q} }
func evidenceFromRow(r db.EvidenceArtifact) (qa.EvidenceArtifact, error) {
	ev := qa.EvidenceArtifact{ID: uuidToString(r.ArtifactID), RunID: uuidToString(r.RunID), TaskID: uuidToString(r.TaskID), Attempt: r.Attempt, Kind: domain.EvidenceKind(r.Kind), URI: r.Uri, SHA256: r.Sha256, CommitSHA: r.CommitSha, Producer: domain.EvidenceProducer(r.Producer), SessionID: r.ProducerSessionID.String}
	if len(r.Metadata) > 0 {
		if err := json.Unmarshal(r.Metadata, &ev.Metadata); err != nil {
			return ev, fmt.Errorf("decode evidence metadata: %w", err)
		}
	}
	return ev, nil
}
func sessionFromArtifact(r db.EvidenceArtifact) (qa.Session, error) {
	var session qa.Session
	if err := json.Unmarshal(r.Metadata, &session); err != nil {
		return session, fmt.Errorf("decode qa session: %w", err)
	}
	return session, nil
}
func (s *PgQAStore) LaunchSessionTx(ctx context.Context, cmd qa.LaunchCommand, now time.Time, effect func(application.RunSnapshot, string, string) (qa.Session, error)) (qa.Session, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return qa.Session{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	id, err := uuidFromString(cmd.RunID)
	if err != nil {
		return qa.Session{}, err
	}
	run, err := q.GetTaskRunForUpdate(ctx, id)
	if err != nil {
		return qa.Session{}, err
	}
	if run.Version != cmd.ExpectedVersion {
		return qa.Session{}, domain.ErrStaleVersion
	}
	if run.State != "qa" {
		return qa.Session{}, domain.ErrQARunNotInQA
	}
	if err := application.CheckRunLease(snapshotFromTaskRun(run), cmd.LeaseOwner, cmd.LeaseEpoch, now); err != nil {
		return qa.Session{}, err
	}
	existing, err := q.GetQASessionArtifact(ctx, db.GetQASessionArtifactParams{RunID: id, Attempt: run.Attempt})
	if err == nil {
		session, e := sessionFromArtifact(existing)
		if e != nil {
			return session, e
		}
		if session.CommitSHA != run.CommitSha.String {
			return session, domain.ErrQASHAMismatch
		}
		return session, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return qa.Session{}, err
	}
	coding, err := q.GetRunMetadataValue(ctx, db.GetRunMetadataValueParams{RunID: id, MetaKey: "coding_session_id"})
	if err != nil {
		return qa.Session{}, fmt.Errorf("coding session snapshot: %w", err)
	}
	codingID, ok := coding.(string)
	if !ok || codingID == "" {
		return qa.Session{}, domain.ErrQASessionNotUnique
	}
	wt, err := q.GetWorktreeByRun(ctx, id)
	if err != nil {
		return qa.Session{}, err
	}
	if wt.Status != "allocated" {
		return qa.Session{}, application.ErrWorktreeNotFound
	}
	session, err := effect(snapshotFromTaskRun(run), wt.Path, codingID)
	if err != nil {
		return session, err
	}
	metadata, err := jsonBytes(session)
	if err != nil {
		return session, err
	}
	_, err = q.InsertEvidenceArtifact(ctx, db.InsertEvidenceArtifactParams{RunID: id, TaskID: run.TaskID, Attempt: run.Attempt, Kind: "qa_review", Uri: "qa-session", Sha256: "", CommitSha: session.CommitSHA, Producer: "system", ProducerSessionID: textFromOptional(session.ID), Metadata: metadata})
	if err != nil {
		return session, err
	}
	if err = tx.Commit(ctx); err != nil {
		return session, err
	}
	return session, nil
}
func (s *PgQAStore) SubmitVerdictTx(ctx context.Context, c qa.SubmitCommand, now time.Time) (qa.VerdictRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return qa.VerdictRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	id, err := uuidFromString(c.RunID)
	if err != nil {
		return qa.VerdictRecord{}, err
	}
	run, err := q.GetTaskRunForUpdate(ctx, id)
	if err != nil {
		return qa.VerdictRecord{}, err
	}
	artifact, err := q.GetQASessionArtifact(ctx, db.GetQASessionArtifactParams{RunID: id, Attempt: run.Attempt})
	if errors.Is(err, pgx.ErrNoRows) {
		return qa.VerdictRecord{}, domain.ErrQASessionNotUnique
	}
	if err != nil {
		return qa.VerdictRecord{}, err
	}
	session, err := sessionFromArtifact(artifact)
	if err != nil {
		return qa.VerdictRecord{}, err
	}
	ids := make([]pgtype.UUID, 0, len(c.EvidenceIDs))
	for _, value := range c.EvidenceIDs {
		evID, e := uuidFromString(value)
		if e != nil {
			return qa.VerdictRecord{}, domain.ErrQAEvidenceMissing
		}
		ids = append(ids, evID)
	}
	rows, err := q.ListEvidenceByIDs(ctx, ids)
	if err != nil {
		return qa.VerdictRecord{}, err
	}
	evidence := make([]qa.EvidenceArtifact, 0, len(rows))
	for _, row := range rows {
		ev, e := evidenceFromRow(row)
		if e != nil {
			return qa.VerdictRecord{}, e
		}
		evidence = append(evidence, ev)
	}
	if err := qa.ValidateSubmission(snapshotFromTaskRun(run), session, c, evidence, now); err != nil {
		return qa.VerdictRecord{}, err
	}
	findings := c.Findings
	if findings == nil {
		findings = []domain.QAFinding{}
	}
	encoded, err := jsonBytes(findings)
	if err != nil {
		return qa.VerdictRecord{}, err
	}
	row, err := q.InsertQAVerdict(ctx, db.InsertQAVerdictParams{RunID: id, TaskID: run.TaskID, Attempt: run.Attempt, CommitSha: c.CommitSHA, Verdict: string(c.Verdict), Findings: encoded, QaSessionID: c.QASessionID, QaAgentIdentity: c.QAIdentity, CodingSessionID: textFromOptional(session.CodingSessionID), EvaluatedAt: timestamptzFrom(&now), CorrelationID: run.CorrelationID})
	if isUniqueViolation(err) {
		return qa.VerdictRecord{}, domain.ErrQAVerdictConflict
	}
	if err != nil {
		return qa.VerdictRecord{}, err
	}
	for _, evID := range ids {
		if err = q.InsertQAVerdictEvidenceLink(ctx, db.InsertQAVerdictEvidenceLinkParams{VerdictID: row.VerdictID, ArtifactID: evID}); err != nil {
			return qa.VerdictRecord{}, err
		}
	}
	record, err := verdictFromRow(row)
	if err != nil {
		return record, err
	}
	record.EvidenceIDs = c.EvidenceIDs
	if err = insertEvidenceEvent(ctx, q, run, "qa.verdict_recorded.v1", map[string]any{"verdict_id": record.ID, "attempt": record.Attempt, "commit_sha": record.CommitSHA, "verdict": record.Verdict, "qa_session_id": record.QASessionID, "qa_agent_identity": record.QAIdentity, "coding_session_id": record.CodingSessionID, "evidence_ids": record.EvidenceIDs, "findings": record.Findings, "evaluated_at": record.EvaluatedAt.Format(time.RFC3339Nano)}, now); err != nil {
		return record, err
	}
	if err = tx.Commit(ctx); err != nil {
		return record, err
	}
	return record, nil
}
func verdictFromRow(r db.QaVerdict) (qa.VerdictRecord, error) {
	record := qa.VerdictRecord{ID: uuidToString(r.VerdictID), RunID: uuidToString(r.RunID), Attempt: r.Attempt, CommitSHA: r.CommitSha, Verdict: domain.QAVerdict(r.Verdict), QASessionID: r.QaSessionID, CodingSessionID: r.CodingSessionID.String, QAIdentity: r.QaAgentIdentity, EvaluatedAt: r.EvaluatedAt.Time}
	if err := json.Unmarshal(r.Findings, &record.Findings); err != nil {
		return record, fmt.Errorf("decode qa findings: %w", err)
	}
	return record, nil
}
func (s *PgQAStore) VerdictForSHA(ctx context.Context, runID, sha string) (*qa.VerdictRecord, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return nil, err
	}
	return loadVerdict(ctx, s.queries, id, sha)
}
func loadVerdict(ctx context.Context, q *db.Queries, id pgtype.UUID, sha string) (*qa.VerdictRecord, error) {
	row, err := q.LatestQAVerdictForSHA(ctx, db.LatestQAVerdictForSHAParams{RunID: id, CommitSha: sha})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := verdictFromRow(row)
	if err != nil {
		return nil, err
	}
	ids, err := q.ListQAVerdictEvidenceIDs(ctx, row.VerdictID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		r.EvidenceIDs = append(r.EvidenceIDs, uuidToString(id))
	}
	return &r, nil
}
func (s *PgQAStore) ListEvidence(ctx context.Context, runID, sha string) ([]qa.EvidenceArtifact, error) {
	id, err := uuidFromString(runID)
	if err != nil {
		return nil, err
	}
	return loadEvidence(ctx, s.queries, id, sha)
}
func loadEvidence(ctx context.Context, q *db.Queries, id pgtype.UUID, sha string) ([]qa.EvidenceArtifact, error) {
	rows, err := q.ListEvidenceForSHA(ctx, db.ListEvidenceForSHAParams{RunID: id, CommitSha: sha})
	if err != nil {
		return nil, err
	}
	result := make([]qa.EvidenceArtifact, 0, len(rows))
	for _, row := range rows {
		ev, err := evidenceFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, ev)
	}
	return result, nil
}
func (s *PgQAStore) RecordEvidence(ctx context.Context, ev qa.EvidenceArtifact) (qa.EvidenceArtifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ev, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	id, err := uuidFromString(ev.RunID)
	if err != nil {
		return ev, err
	}
	run, err := q.GetTaskRunForUpdate(ctx, id)
	if err != nil {
		return ev, err
	}
	if run.CommitSha.String != ev.CommitSHA || run.Attempt != ev.Attempt || run.State == "failed" || run.State == "cancelled" {
		return ev, domain.ErrQASHAMismatch
	}
	existing, err := q.FindEvidenceArtifactByContent(ctx, db.FindEvidenceArtifactByContentParams{RunID: id, Uri: ev.URI, Sha256: ev.SHA256, CommitSha: ev.CommitSHA, Attempt: ev.Attempt, Producer: string(ev.Producer), SessionID: textFromOptional(ev.SessionID)})
	if err == nil {
		return evidenceFromRow(existing)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ev, err
	}
	metadata, err := jsonBytes(ev.Metadata)
	if err != nil {
		return ev, err
	}
	row, err := q.InsertEvidenceArtifact(ctx, db.InsertEvidenceArtifactParams{RunID: id, TaskID: run.TaskID, Attempt: ev.Attempt, Kind: string(ev.Kind), Uri: ev.URI, Sha256: ev.SHA256, CommitSha: ev.CommitSHA, Producer: string(ev.Producer), ProducerSessionID: textFromOptional(ev.SessionID), Metadata: metadata})
	if err != nil {
		return ev, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ev, err
	}
	return evidenceFromRow(row)
}
func insertEvidenceEvent(ctx context.Context, q *db.Queries, run db.TaskRun, eventType string, body map[string]any, now time.Time) error {
	body["run_id"] = uuidToString(run.RunID)
	body["task_id"] = uuidToString(run.TaskID)
	body["intent_id"] = uuidToString(run.IntentID)
	body["correlation_id"] = run.CorrelationID.String
	body["occurred_at"] = now.Format(time.RFC3339Nano)
	payload, err := jsonBytes(body)
	if err != nil {
		return err
	}
	_, err = q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{IntentID: run.IntentID, EventType: eventType, Payload: payload})
	return err
}
