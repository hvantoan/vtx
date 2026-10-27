package gate

import (
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/qa"
	"reflect"
	"testing"
	"time"
)

func greenInput() Input {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Minute)
	sha, owner := "head", "worker"
	return Input{Run: application.RunSnapshot{RunID: "run", State: domain.StateQualityGate, Attempt: 1, CommitSHA: &sha, LeaseOwner: &owner, LeaseEpoch: 2, LeaseExpiresAt: &expiry}, HeadSHA: sha, ExpectedLeaseEpoch: 2, Now: now, Evidence: []qa.EvidenceArtifact{{ID: "verification", RunID: "run", Attempt: 1, CommitSHA: sha, Kind: domain.EvidenceTestResults, Producer: domain.ProducerVerifier, Metadata: map[string]any{"status": "all_green"}}, {ID: "qa-tests", RunID: "run", Attempt: 1, CommitSHA: sha, Kind: domain.EvidenceQATestRun, Producer: domain.ProducerQAAgent, SessionID: "qa"}}, Verdict: &qa.VerdictRecord{ID: "verdict", RunID: "run", Attempt: 1, CommitSHA: sha, Verdict: domain.QAVerdictPass, QASessionID: "qa", CodingSessionID: "coding", QAIdentity: "qa-agent", EvidenceIDs: []string{"qa-tests"}}}
}
func TestGatePolicyMandatoryConditions(t *testing.T) {
	for _, tc := range []struct {
		name, condition string
		mutate          func(*Input)
	}{
		{"wrong_state", "run_in_quality_gate", func(i *Input) { i.Run.State = domain.StateQA }},
		{"head_moved", "sha_binding", func(i *Input) { i.HeadSHA = "other" }},
		{"missing_sha", "sha_binding", func(i *Input) { i.Run.CommitSHA = nil }},
		{"missing_verification", "verification_green", func(i *Input) { i.Evidence = i.Evidence[1:] }},
		{"red_verification", "verification_green", func(i *Input) { i.Evidence[0].Metadata["status"] = "failed" }},
		{"stale_verification", "verification_green", func(i *Input) { i.Evidence[0].CommitSHA = "old" }},
		{"missing_qa", "qa_verdict_pass", func(i *Input) { i.Verdict = nil }},
		{"failed_qa", "qa_verdict_pass", func(i *Input) { i.Verdict.Verdict = domain.QAVerdictFail }},
		{"inconclusive_qa", "qa_verdict_pass", func(i *Input) { i.Verdict.Verdict = domain.QAVerdictInconclusive }},
		{"self_review", "qa_verdict_pass", func(i *Input) { i.Verdict.QASessionID = i.Verdict.CodingSessionID }},
		{"stale_verdict", "qa_verdict_pass", func(i *Input) { i.Verdict.CommitSHA = "old" }},
		{"missing_qa_tests", "qa_executed_tests", func(i *Input) { i.Evidence = i.Evidence[:1] }},
		{"unlinked_tests", "qa_executed_tests", func(i *Input) { i.Verdict.EvidenceIDs = nil }},
		{"coding_agent_tests", "qa_executed_tests", func(i *Input) { i.Evidence[1].Producer = domain.ProducerCodingAgent }},
		{"stale_qa_tests", "qa_executed_tests", func(i *Input) { i.Evidence[1].CommitSHA = "old" }},
		{"lease_expired", "lease_fencing", func(i *Input) { i.Run.LeaseExpiresAt = &i.Now }},
		{"lease_fenced", "lease_fencing", func(i *Input) { i.ExpectedLeaseEpoch++ }},
		{"prior_attempt", "no_superseding_attempt", func(i *Input) { i.Verdict.Attempt-- }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := greenInput()
			tc.mutate(&in)
			d := Evaluate(in)
			if d.Decision != "fail" || len(d.Conditions) != 7 {
				t.Fatalf("%+v", d)
			}
			for _, c := range d.Conditions {
				if c.Name == tc.condition {
					if c.Satisfied {
						t.Fatalf("%s passed", c.Name)
					}
					return
				}
			}
			t.Fatal("missing condition")
		})
	}
}
func TestGatePolicyDeterministicAndIgnoresNarrative(t *testing.T) {
	in := greenInput()
	a, b := Evaluate(in), Evaluate(in)
	if a.Decision != "pass" || !reflect.DeepEqual(a, b) {
		t.Fatalf("%+v %+v", a, b)
	}
	in.Verdict.Findings = []domain.QAFinding{{Summary: "LLM says fail"}}
	if !reflect.DeepEqual(a, Evaluate(in)) {
		t.Fatal("narrative affected policy")
	}
}
