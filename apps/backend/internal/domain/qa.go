package domain

import "errors"

// VTX-9/VTX-10 domain vocabulary. Wire values are contract-frozen: they appear
// in CHECK constraints and event payloads.

// QAVerdict is the persisted QA outcome (qa_verdicts.verdict).
type QAVerdict string

const (
	QAVerdictPass         QAVerdict = "pass"
	QAVerdictFail         QAVerdict = "fail"
	QAVerdictInconclusive QAVerdict = "inconclusive"
)

func (v QAVerdict) Valid() bool {
	switch v {
	case QAVerdictPass, QAVerdictFail, QAVerdictInconclusive:
		return true
	default:
		return false
	}
}

// EvidenceKind is evidence_artifacts.kind.
type EvidenceKind string

const (
	EvidenceTestResults EvidenceKind = "test_results"
	EvidenceBuildLog    EvidenceKind = "build_log"
	EvidenceLintReport  EvidenceKind = "lint_report"
	EvidenceCoverage    EvidenceKind = "coverage"
	EvidenceQAReview    EvidenceKind = "qa_review"
	EvidenceQATestRun   EvidenceKind = "qa_test_run"
	EvidenceCIStatus    EvidenceKind = "ci_status"
	EvidenceDiffSummary EvidenceKind = "diff_summary"
)

// EvidenceProducer is evidence_artifacts.producer.
type EvidenceProducer string

const (
	ProducerCodingAgent EvidenceProducer = "coding_agent"
	ProducerVerifier    EvidenceProducer = "verifier"
	ProducerQAAgent     EvidenceProducer = "qa_agent"
	ProducerGitHubCI    EvidenceProducer = "github_ci"
	ProducerSystem      EvidenceProducer = "system"
)

// QAFinding is one structured QA finding (qa_verdicts.findings[]). Free-form
// narrative is metadata, never a gate input (FR-QA-002).
type QAFinding struct {
	Severity            string `json:"severity"` // blocker|major|minor|info
	Category            string `json:"category"`
	Summary             string `json:"summary"`
	Path                string `json:"path,omitempty"`
	Line                int    `json:"line,omitempty"`
	EvidenceArtifactID  string `json:"evidence_artifact_id,omitempty"`
}

var (
	// ErrQAVerdictConflict — a verdict already exists for (run_id, attempt). 409.
	ErrQAVerdictConflict = errors.New("qa: verdict already recorded for attempt")
	// ErrQASessionNotUnique — the QA session equals the coding session;
	// independence (FR-QA-001) forbids self-review. 409.
	ErrQASessionNotUnique = errors.New("qa: qa session identical to coding session")
	// ErrQAEvidenceMissing — the mandatory qa_test_run artifact produced by
	// the QA agent is absent (or bound to a different SHA/run). 409.
	ErrQAEvidenceMissing = errors.New("qa: mandatory qa_test_run evidence absent")
	// ErrQASHAMismatch — the verdict's commit_sha differs from the run's
	// recorded commit_sha (FR-QA-003). 409.
	ErrQASHAMismatch = errors.New("qa: verdict commit_sha does not match run")
	// ErrQARunNotInQA — verdict submissions are only accepted while the run
	// is parked in the qa stage. 409.
	ErrQARunNotInQA = errors.New("qa: run is not in the qa stage")
	// ErrEvidenceInvalid is the VTX-10 guard seam: the hop is contract-valid
	// and its downstream system is deployed, but the required evidence is
	// absent, stale, or bound to a different SHA. 409 — distinct from
	// ErrStageNotOperational (system absent).
	ErrEvidenceInvalid = errors.New("registry: required evidence missing or stale")
)
