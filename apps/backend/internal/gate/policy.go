// Package gate implements vtx-gate-policy/v1: seven mandatory, objective
// conditions. PASS is conjunction, never a score or a narrative judgment.
package gate

import (
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/qa"
	"time"
)

type Condition struct {
	Name        string   `json:"name"`
	Satisfied   bool     `json:"satisfied"`
	Blocking    bool     `json:"blocking"`
	Detail      string   `json:"detail,omitempty"`
	EvidenceIDs []string `json:"evidence_artifact_ids,omitempty"`
}
type Decision struct {
	ID               string      `json:"decision_id"`
	RunID            string      `json:"run_id"`
	Attempt          int32       `json:"attempt"`
	SubjectCommitSHA string      `json:"subject_commit_sha"`
	VerifiedHeadSHA  string      `json:"verified_head_sha"`
	PolicyName       string      `json:"policy_name"`
	PolicyVersion    string      `json:"policy_version"`
	Decision         string      `json:"decision"`
	Conditions       []Condition `json:"conditions"`
	BlockingReasons  []string    `json:"blocking_reasons"`
	LeaseEpoch       int64       `json:"lease_epoch"`
	EvaluatedAt      time.Time   `json:"evaluated_at"`
}
type Input struct {
	Run                application.RunSnapshot
	HeadSHA            string
	Evidence           []qa.EvidenceArtifact
	Verdict            *qa.VerdictRecord
	Now                time.Time
	ExpectedLeaseEpoch int64
}

func VerificationEvidence(run application.RunSnapshot, ev []qa.EvidenceArtifact) []string {
	ids := []string{}
	for _, e := range ev {
		if run.CommitSHA != nil && e.RunID == run.RunID && e.CommitSHA == *run.CommitSHA && e.Attempt == run.Attempt && e.Kind == domain.EvidenceTestResults && e.Producer == domain.ProducerVerifier && e.Metadata["status"] == "all_green" {
			ids = append(ids, e.ID)
		}
	}
	return ids
}
func Evaluate(in Input) Decision {
	r := in.Run
	sha := ""
	if r.CommitSHA != nil {
		sha = *r.CommitSHA
	}
	d := Decision{RunID: r.RunID, Attempt: r.Attempt, SubjectCommitSHA: sha, VerifiedHeadSHA: in.HeadSHA, PolicyName: "vtx-gate-policy", PolicyVersion: "v1", Decision: "fail", LeaseEpoch: r.LeaseEpoch, EvaluatedAt: in.Now, BlockingReasons: []string{}}
	green := VerificationEvidence(r, in.Evidence)
	v := in.Verdict
	verdictPass := v != nil && v.RunID == r.RunID && v.CommitSHA == sha && v.Verdict == domain.QAVerdictPass && v.QASessionID != "" && v.CodingSessionID != "" && v.QASessionID != v.CodingSessionID && v.QAIdentity == "qa-agent"
	qaIDs := []string{}
	if v != nil {
		links := map[string]bool{}
		for _, id := range v.EvidenceIDs {
			links[id] = true
		}
		for _, e := range in.Evidence {
			if links[e.ID] && e.RunID == r.RunID && e.CommitSHA == sha && e.Attempt == r.Attempt && e.Kind == domain.EvidenceQATestRun && e.Producer == domain.ProducerQAAgent && e.SessionID == v.QASessionID {
				qaIDs = append(qaIDs, e.ID)
			}
		}
	}
	lease := r.LeaseOwner != nil && *r.LeaseOwner != "" && r.LeaseExpiresAt != nil && r.LeaseExpiresAt.After(in.Now) && r.LeaseEpoch == in.ExpectedLeaseEpoch
	d.Conditions = []Condition{
		{Name: "run_in_quality_gate", Satisfied: r.State == domain.StateQualityGate, Blocking: true},
		{Name: "sha_binding", Satisfied: sha != "" && sha == in.HeadSHA, Blocking: true},
		{Name: "verification_green", Satisfied: len(green) > 0, Blocking: true, EvidenceIDs: green},
		{Name: "qa_verdict_pass", Satisfied: verdictPass, Blocking: true},
		{Name: "qa_executed_tests", Satisfied: len(qaIDs) > 0, Blocking: true, EvidenceIDs: qaIDs},
		{Name: "lease_fencing", Satisfied: lease, Blocking: true, Detail: "lease epoch recorded on decision"},
		{Name: "no_superseding_attempt", Satisfied: v != nil && v.Attempt == r.Attempt, Blocking: true},
	}
	// All conditions are cheap and evaluated, making every failed predicate
	// visible instead of omitting the tail after the first blocking failure.
	for _, c := range d.Conditions {
		if !c.Satisfied {
			d.BlockingReasons = append(d.BlockingReasons, c.Name)
		}
	}
	if len(d.BlockingReasons) == 0 {
		d.Decision = "pass"
	}
	return d
}
