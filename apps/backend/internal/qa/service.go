// Package qa records independent, exact-SHA QA outcomes. Narrative never
// substitutes for QA-executed test evidence.
package qa

import (
	"context"
	"fmt"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/gitops"
)

type EvidenceArtifact struct {
	ID        string                  `json:"artifact_id"`
	RunID     string                  `json:"run_id"`
	TaskID    string                  `json:"task_id"`
	Attempt   int32                   `json:"attempt"`
	Kind      domain.EvidenceKind     `json:"kind"`
	URI       string                  `json:"uri"`
	SHA256    string                  `json:"sha256"`
	CommitSHA string                  `json:"commit_sha"`
	Producer  domain.EvidenceProducer `json:"producer"`
	SessionID string                  `json:"producer_session_id,omitempty"`
	Metadata  map[string]any          `json:"metadata,omitempty"`
}
type VerdictRecord struct {
	ID              string             `json:"verdict_id"`
	RunID           string             `json:"run_id"`
	Attempt         int32              `json:"attempt"`
	CommitSHA       string             `json:"commit_sha"`
	Verdict         domain.QAVerdict   `json:"verdict"`
	Findings        []domain.QAFinding `json:"findings"`
	QASessionID     string             `json:"qa_session_id"`
	CodingSessionID string             `json:"coding_session_id"`
	QAIdentity      string             `json:"qa_agent_identity"`
	EvidenceIDs     []string           `json:"evidence_ids"`
	EvaluatedAt     time.Time          `json:"evaluated_at"`
}
type LaunchCommand struct {
	RunID           string `json:"-"`
	ExpectedVersion int64  `json:"expected_version"`
	LeaseOwner      string `json:"lease_owner"`
	LeaseEpoch      int64  `json:"lease_epoch"`
}
type SubmitCommand struct {
	RunID       string             `json:"-"`
	CommitSHA   string             `json:"commit_sha"`
	Verdict     domain.QAVerdict   `json:"verdict"`
	Findings    []domain.QAFinding `json:"findings"`
	EvidenceIDs []string           `json:"evidence_ids"`
	QASessionID string             `json:"qa_session_id"`
	QAIdentity  string             `json:"-"`
	LeaseOwner  string             `json:"lease_owner"`
	LeaseEpoch  int64              `json:"lease_epoch"`
}
type Session struct {
	ID              string `json:"session_id"`
	CommitSHA       string `json:"commit_sha"`
	CodingSessionID string `json:"coding_session_id"`
	Path            string `json:"-"`
}

// AgentWorkSpec intentionally carries no coding transcript or self-assessment.
// StartSession MUST honor IdempotencyKey across replay after a failed DB commit.
// Profile=qa enforces read-only tools (test scratch only, no commit or push).
type AgentWorkSpec struct {
	IdempotencyKey, Profile, Principal, Worktree, CommitSHA string
	ReadOnly                                                bool
}
type AgentRunner interface {
	StartSession(context.Context, AgentWorkSpec) (string, error)
	CancelSession(context.Context, string, string) error
}
type Store interface {
	LaunchSessionTx(context.Context, LaunchCommand, time.Time, func(application.RunSnapshot, string, string) (Session, error)) (Session, error)
	SubmitVerdictTx(context.Context, SubmitCommand, time.Time) (VerdictRecord, error)
	VerdictForSHA(context.Context, string, string) (*VerdictRecord, error)
	ListEvidence(context.Context, string, string) ([]EvidenceArtifact, error)
	RecordEvidence(context.Context, EvidenceArtifact) (EvidenceArtifact, error)
}
type Service struct {
	Store  Store
	Runner AgentRunner
	Git    application.HeadReader
	Layout gitops.Layout
	Clock  application.Clock
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now()
}
func (s *Service) LaunchSession(ctx context.Context, cmd LaunchCommand) (Session, error) {
	if s.Runner == nil {
		return Session{}, fmt.Errorf("qa runtime unavailable: %w", domain.ErrStageNotOperational)
	}
	return s.Store.LaunchSessionTx(ctx, cmd, s.now(), func(run application.RunSnapshot, path, coding string) (Session, error) {
		path, err := gitops.CanonicalPath(s.Layout.WorktreeRoot, path)
		if err != nil {
			return Session{}, err
		}
		sha, err := s.Git.HeadSHA(ctx, path)
		if err != nil {
			return Session{}, err
		}
		if run.CommitSHA == nil || sha != *run.CommitSHA {
			return Session{}, domain.ErrQASHAMismatch
		}
		id, err := s.Runner.StartSession(ctx, AgentWorkSpec{IdempotencyKey: fmt.Sprintf("qa/%s/%d", run.RunID, run.Attempt), Profile: "qa", Principal: "qa-agent", Worktree: path, CommitSHA: sha, ReadOnly: true})
		if err != nil {
			return Session{}, fmt.Errorf("launch qa: %w", err)
		}
		if id == "" || id == coding {
			if e := s.Runner.CancelSession(ctx, id, "session_not_independent"); e != nil {
				return Session{}, fmt.Errorf("cancel invalid qa session: %w", e)
			}
			return Session{}, domain.ErrQASessionNotUnique
		}
		return Session{ID: id, CommitSHA: sha, CodingSessionID: coding, Path: path}, nil
	})
}
func (s *Service) SubmitVerdict(ctx context.Context, c SubmitCommand) (VerdictRecord, error) {
	if !c.Verdict.Valid() {
		return VerdictRecord{}, domain.ErrInvalidTransition
	}
	return s.Store.SubmitVerdictTx(ctx, c, s.now())
}
func (s *Service) VerdictForSHA(ctx context.Context, run, sha string) (*VerdictRecord, error) {
	return s.Store.VerdictForSHA(ctx, run, sha)
}
func (s *Service) ListEvidence(ctx context.Context, run, sha string) ([]EvidenceArtifact, error) {
	return s.Store.ListEvidence(ctx, run, sha)
}

// ValidateSubmission runs against locked run/session data, never caller-supplied
// coding identity. Every linked artifact must belong to this run, SHA, attempt.
func ValidateSubmission(run application.RunSnapshot, session Session, c SubmitCommand, evidence []EvidenceArtifact, now time.Time) error {
	if run.State != domain.StateQA {
		return domain.ErrQARunNotInQA
	}
	if err := application.CheckRunLease(run, c.LeaseOwner, c.LeaseEpoch, now); err != nil {
		return err
	}
	if run.CommitSHA == nil || *run.CommitSHA == "" || c.CommitSHA != *run.CommitSHA || session.CommitSHA != c.CommitSHA {
		return domain.ErrQASHAMismatch
	}
	if session.ID == "" || session.CodingSessionID == "" || c.QASessionID != session.ID || c.QASessionID == session.CodingSessionID || c.QAIdentity != "qa-agent" {
		return domain.ErrQASessionNotUnique
	}
	if !c.Verdict.Valid() {
		return domain.ErrInvalidTransition
	}
	found := false
	ids := map[string]bool{}
	for _, id := range c.EvidenceIDs {
		ids[id] = true
	}
	for _, ev := range evidence {
		if !ids[ev.ID] || ev.RunID != run.RunID || ev.CommitSHA != c.CommitSHA || ev.Attempt != run.Attempt {
			return domain.ErrQAEvidenceMissing
		}
		delete(ids, ev.ID)
		if ev.Producer == domain.ProducerQAAgent && ev.Kind == domain.EvidenceQATestRun && ev.SessionID == c.QASessionID {
			found = true
		}
	}
	if !found || len(ids) != 0 {
		return domain.ErrQAEvidenceMissing
	}
	return nil
}
