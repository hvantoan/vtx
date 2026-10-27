// Package launcher drives the orchestrator-owned stages: it finds runs that
// the scheduler admitted and preparing made dispatch-ready, then spawns one
// headless Pi orchestrator per run with a fenced, untrusted-data RunSpec.
//
// The launcher is deliberately dumb about the pipeline: it never decides a
// gate or forges evidence. Every authoritative mutation still goes through the
// spine over the unix socket; the launcher only starts and supervises the
// process, and blocks a run that dies without advancing.
package launcher

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/vtx/platform/internal/requirement"
)

// Spec mirrors apps/agent-runtime/src/orchestrator/spec.ts field for field.
// The agent runtime validates it and forces qa_independent true regardless.
type Spec struct {
	RunID              string     `json:"run_id"`
	RunVersion         int64      `json:"run_version"`
	RunState           string     `json:"run_state"`
	Goal               string     `json:"goal"`
	AcceptanceCriteria []string   `json:"acceptance_criteria"`
	Repo               SpecRepo   `json:"repo"`
	TestBaseline       string     `json:"test_baseline"`
	AllowedTools       []string   `json:"allowed_tools"`
	Delegation         Delegation `json:"delegation"`
	Budget             Budget     `json:"budget"`
	StopConditions     []string   `json:"stop_conditions"`
	Lease              Lease      `json:"lease"`
}

type SpecRepo struct {
	URL          string `json:"url"`
	BaseRef      string `json:"base_ref"`
	WorktreePath string `json:"worktree_path"`
}

type Delegation struct {
	Agents        []string `json:"agents"`
	MaxParallel   int      `json:"max_parallel"`
	QAIndependent bool     `json:"qa_independent"`
}

type Budget struct {
	MaxTurns       int   `json:"max_turns"`
	MaxTokens      int   `json:"max_tokens"`
	WallClockMS    int64 `json:"wall_clock_ms"`
	MaxDelegations int   `json:"max_delegations"`
}

type Lease struct {
	Owner           string `json:"owner"`
	Epoch           int64  `json:"epoch"`
	ExpiresAt       string `json:"expires_at"`
	RenewIntervalMS int64  `json:"renew_interval_ms"`
}

// AllowedTools is the orchestrator tool universe mirrored from spec.ts. The
// agent runtime intersects the spec with the same list, so this is an
// allowlist of what the launcher is willing to grant — never a widening.
var AllowedTools = []string{
	"vtx_plan",
	"vtx_verify",
	"vtx_qa",
	"vtx_gate",
	"vtx_open_pr",
	"vtx_advance",
	"vtx_block",
	"vtx_escalate",
}

// DefaultAgents are the specialists the orchestrator may delegate coding and
// review to. QA is deliberately absent: it is launched through the spine so
// independence is structural.
var DefaultAgents = []string{"vtx-coder", "vtx-reviewer"}

// DefaultStopConditions are fixed invariants, not model choices.
var DefaultStopConditions = []string{
	"quality gate returns FAIL",
	"budget exhausted",
	"lease lost or fenced",
	"run cancelled or blocked by an operator",
}

// BuildSpec turns committed preparation evidence into the RunSpec both the
// launcher and the agent runtime treat as data. It fails closed when the run is
// missing the lease or the dispatch inputs, so a half-prepared run never starts.
func BuildSpec(in requirement.Input, packet requirement.Packet, baseline requirement.Baseline, cfg Config) (Spec, error) {
	cfg = cfg.defaults()
	if in.Run.RunID == "" {
		return Spec{}, fmt.Errorf("launcher: run_id is empty")
	}
	if in.Run.LeaseOwner == nil || in.Run.LeaseExpiresAt == nil {
		return Spec{}, fmt.Errorf("launcher: run %s has no lease", in.Run.RunID)
	}
	if in.Run.LeaseEpoch <= 0 {
		return Spec{}, fmt.Errorf("launcher: run %s has a non-fenced lease epoch", in.Run.RunID)
	}
	if in.Worktree.Path == "" || in.Worktree.RepoURL == "" {
		return Spec{}, fmt.Errorf("launcher: run %s has no allocated worktree", in.Run.RunID)
	}
	baseRef := in.Worktree.Branch
	if in.Worktree.BaseSHA != nil && *in.Worktree.BaseSHA != "" {
		baseRef = *in.Worktree.BaseSHA
	}
	if baseRef == "" {
		return Spec{}, fmt.Errorf("launcher: run %s has no base ref", in.Run.RunID)
	}
	goal := strings.TrimSpace(packet.Summary)
	if goal == "" {
		goal = strings.TrimSpace(packet.Description)
	}
	if goal == "" {
		goal = "Implement " + packet.IssueKey
	}
	suite := strings.TrimSpace(baseline.Suite)
	if suite == "" {
		suite = cfg.TestSuite
	}
	testBaseline := suite
	if baseline.Status != "" {
		testBaseline = fmt.Sprintf("%s (baseline: %s)", suite, baseline.Status)
	}
	criteria := packet.AcceptanceCriteria
	if criteria == nil {
		criteria = []string{}
	}
	return Spec{
		RunID:              in.Run.RunID,
		RunVersion:         in.Run.Version,
		RunState:           string(in.Run.State),
		Goal:               goal,
		AcceptanceCriteria: criteria,
		Repo: SpecRepo{
			URL:          in.Worktree.RepoURL,
			BaseRef:      baseRef,
			WorktreePath: in.Worktree.Path,
		},
		TestBaseline: testBaseline,
		AllowedTools: append([]string(nil), AllowedTools...),
		Delegation: Delegation{
			Agents:        append([]string(nil), cfg.Agents...),
			MaxParallel:   cfg.MaxParallel,
			QAIndependent: true,
		},
		Budget: Budget{
			MaxTurns:       cfg.Budget.MaxTurns,
			MaxTokens:      cfg.Budget.MaxTokens,
			WallClockMS:    cfg.Budget.WallClockMS,
			MaxDelegations: cfg.Budget.MaxDelegations,
		},
		StopConditions: append([]string(nil), DefaultStopConditions...),
		Lease: Lease{
			Owner:           *in.Run.LeaseOwner,
			Epoch:           in.Run.LeaseEpoch,
			ExpiresAt:       in.Run.LeaseExpiresAt.Format(time.RFC3339),
			RenewIntervalMS: cfg.LeaseRenew.Milliseconds(),
		},
	}, nil
}

// newClaimToken is the per-launch identity written into the claim row. A
// relauncher that took over after the stale window holds a different token.
func newClaimToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("launcher: claim token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
