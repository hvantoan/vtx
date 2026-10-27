package qa

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/gitops"
)

func TestValidateSubmissionExactSHAIndependenceAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*application.RunSnapshot, *Session, *SubmitCommand, *[]EvidenceArtifact)
		want   error
	}{
		{"pass", func(*application.RunSnapshot, *Session, *SubmitCommand, *[]EvidenceArtifact) {}, nil},
		{"stale_sha", func(_ *application.RunSnapshot, _ *Session, c *SubmitCommand, _ *[]EvidenceArtifact) {
			c.CommitSHA = "old"
		}, domain.ErrQASHAMismatch},
		{"stale_session_sha", func(_ *application.RunSnapshot, s *Session, _ *SubmitCommand, _ *[]EvidenceArtifact) {
			s.CommitSHA = "old"
		}, domain.ErrQASHAMismatch},
		{"same_session", func(_ *application.RunSnapshot, s *Session, _ *SubmitCommand, _ *[]EvidenceArtifact) {
			s.CodingSessionID = "qa"
		}, domain.ErrQASessionNotUnique},
		{"missing_coding_identity", func(_ *application.RunSnapshot, s *Session, _ *SubmitCommand, _ *[]EvidenceArtifact) {
			s.CodingSessionID = ""
		}, domain.ErrQASessionNotUnique},
		{"missing_tests", func(_ *application.RunSnapshot, _ *Session, _ *SubmitCommand, ev *[]EvidenceArtifact) { *ev = nil }, domain.ErrQAEvidenceMissing},
		{"coding_output_not_qa", func(_ *application.RunSnapshot, _ *Session, _ *SubmitCommand, ev *[]EvidenceArtifact) {
			(*ev)[0].Producer = domain.ProducerCodingAgent
		}, domain.ErrQAEvidenceMissing},
		{"stale_evidence", func(_ *application.RunSnapshot, _ *Session, _ *SubmitCommand, ev *[]EvidenceArtifact) {
			(*ev)[0].CommitSHA = "old"
		}, domain.ErrQAEvidenceMissing},
		{"prior_attempt", func(_ *application.RunSnapshot, _ *Session, _ *SubmitCommand, ev *[]EvidenceArtifact) {
			(*ev)[0].Attempt = 0
		}, domain.ErrQAEvidenceMissing},
		{"foreign_run", func(_ *application.RunSnapshot, _ *Session, _ *SubmitCommand, ev *[]EvidenceArtifact) {
			(*ev)[0].RunID = "other"
		}, domain.ErrQAEvidenceMissing},
		{"forged_session", func(_ *application.RunSnapshot, _ *Session, c *SubmitCommand, _ *[]EvidenceArtifact) {
			c.QASessionID = "forged"
		}, domain.ErrQASessionNotUnique},
		{"stale_lease", func(r *application.RunSnapshot, _ *Session, _ *SubmitCommand, _ *[]EvidenceArtifact) { r.LeaseEpoch++ }, domain.ErrStaleLease},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			expiry := now.Add(time.Minute)
			sha, owner := "head", "owner"
			run := application.RunSnapshot{RunID: "run", State: domain.StateQA, Attempt: 1, CommitSHA: &sha, LeaseOwner: &owner, LeaseEpoch: 1, LeaseExpiresAt: &expiry}
			session := Session{ID: "qa", CommitSHA: sha, CodingSessionID: "coding"}
			cmd := SubmitCommand{CommitSHA: sha, QASessionID: "qa", QAIdentity: "qa-agent", Verdict: domain.QAVerdictPass, EvidenceIDs: []string{"tests"}, LeaseOwner: owner, LeaseEpoch: 1}
			ev := []EvidenceArtifact{{ID: "tests", RunID: "run", Attempt: 1, CommitSHA: sha, Kind: domain.EvidenceQATestRun, Producer: domain.ProducerQAAgent, SessionID: "qa"}}
			tc.mutate(&run, &session, &cmd, &ev)
			if err := ValidateSubmission(run, session, cmd, ev, now); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

type launchStore struct {
	Store
	run  application.RunSnapshot
	path string
}

func (s launchStore) LaunchSessionTx(_ context.Context, _ LaunchCommand, _ time.Time, effect func(application.RunSnapshot, string, string) (Session, error)) (Session, error) {
	return effect(s.run, s.path, "coding")
}

type launchRunner struct {
	spec  AgentWorkSpec
	calls int
}

func (r *launchRunner) StartSession(_ context.Context, s AgentWorkSpec) (string, error) {
	r.spec = s
	r.calls++
	return "qa", nil
}
func (r *launchRunner) CancelSession(context.Context, string, string) error { return nil }

type head string

func (h head) HeadSHA(context.Context, string) (string, error) { return string(h), nil }
func TestLaunchSessionPinsHeadAndUsesIndependentProfile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "run")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	sha := "head"
	runner := &launchRunner{}
	s := Service{Store: launchStore{run: application.RunSnapshot{RunID: "run", Attempt: 2, CommitSHA: &sha}, path: path}, Runner: runner, Git: head(sha), Layout: gitops.Layout{WorktreeRoot: root}}
	session, err := s.LaunchSession(context.Background(), LaunchCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if session.CommitSHA != sha || runner.spec.Profile != "qa" || runner.spec.Principal != "qa-agent" || !runner.spec.ReadOnly || runner.spec.Worktree != path || runner.spec.IdempotencyKey != "qa/run/2" {
		t.Fatalf("%+v %+v", session, runner.spec)
	}
	s.Git = head("moved")
	if _, err := s.LaunchSession(context.Background(), LaunchCommand{}); !errors.Is(err, domain.ErrQASHAMismatch) {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatal("launched stale QA")
	}
	s.Runner = nil
	if _, err := s.LaunchSession(context.Background(), LaunchCommand{}); !errors.Is(err, domain.ErrStageNotOperational) {
		t.Fatal(err)
	}
}
