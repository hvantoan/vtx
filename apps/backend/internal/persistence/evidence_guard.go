package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/gate"
	"github.com/vtx/platform/internal/gitops"
	"github.com/vtx/platform/internal/persistence/db"
)

// TransitionGuard executes after domain authorization/CAS/lease checks and
// before the state write, on the SAME tx-bound Queries and locked run. VTX-12
// DraftPRGuard plugs into this seam; never read evidence on a pool connection.
type TransitionGuard interface {
	Check(context.Context, *db.Queries, db.TaskRun, application.TransitionCommand, *application.TransitionPlan) error
}
type TransitionKey struct {
	State  domain.State
	Action domain.Action
}

func (s *PgRegistryStore) HasTransitionGuard(state domain.State, action domain.Action) bool {
	return s.guards[TransitionKey{state, action}] != nil
}

// WithTransitionGuards is startup-only. Required guards must all be supplied;
// without opt-in, the existing constructor keeps downstream stages closed.
func (s *PgRegistryStore) WithTransitionGuards(guards map[TransitionKey]TransitionGuard) error {
	for _, state := range []domain.State{domain.StateCoding, domain.StateVerifying, domain.StateQA, domain.StateQualityGate} {
		if guards[TransitionKey{state, domain.ActionAdvance}] == nil {
			return fmt.Errorf("missing transition guard: %s advance", state)
		}
	}
	s.guards = make(map[TransitionKey]TransitionGuard, len(guards))
	for k, g := range guards {
		if g == nil {
			return fmt.Errorf("nil transition guard: %s %s", k.State, k.Action)
		}
		s.guards[k] = g
	}
	return nil
}

type EvidenceGuards struct {
	Head         application.HeadReader
	WorktreeRoot string
	Clock        application.Clock
}

func (g EvidenceGuards) now() time.Time {
	if g.Clock == nil {
		return time.Now().UTC()
	}
	return g.Clock.Now()
}
func (g EvidenceGuards) GuardMap() map[TransitionKey]TransitionGuard {
	m := map[TransitionKey]TransitionGuard{}
	for _, state := range []domain.State{domain.StateCoding, domain.StateVerifying, domain.StateQA, domain.StateQualityGate} {
		m[TransitionKey{state, domain.ActionAdvance}] = g
	}
	return m
}
func (g EvidenceGuards) resolveHead(ctx context.Context, q *db.Queries, run db.TaskRun) (string, error) {
	wt, err := q.GetWorktreeByRun(ctx, run.RunID)
	if err != nil {
		return "", fmt.Errorf("gate worktree: %w", err)
	}
	if wt.Status != "allocated" {
		return "", domain.ErrEvidenceInvalid
	}
	path, err := gitops.CanonicalPath(g.WorktreeRoot, wt.Path)
	if err != nil {
		return "", err
	}
	return g.Head.HeadSHA(ctx, path)
}
func (g EvidenceGuards) Check(ctx context.Context, q *db.Queries, run db.TaskRun, cmd application.TransitionCommand, plan *application.TransitionPlan) error {
	snapshot := snapshotFromTaskRun(run)
	switch snapshot.State {
	case domain.StateCoding:
		sha, err := g.resolveHead(ctx, q, run)
		if err != nil {
			return err
		}
		if !run.CommitSha.Valid || run.CommitSha.String == "" || sha != run.CommitSha.String {
			return domain.ErrEvidenceInvalid
		}
	case domain.StateVerifying:
		ev, err := loadEvidence(ctx, q, run.RunID, run.CommitSha.String)
		if err != nil {
			return err
		}
		if len(gate.VerificationEvidence(snapshot, ev)) == 0 {
			return domain.ErrEvidenceInvalid
		}
	case domain.StateQA:
		verdict, err := loadVerdict(ctx, q, run.RunID, run.CommitSha.String)
		if err != nil {
			return err
		}
		if verdict == nil || verdict.Attempt != run.Attempt || (verdict.Verdict != domain.QAVerdictPass && verdict.Verdict != domain.QAVerdictFail) {
			return domain.ErrEvidenceInvalid
		}
		return addAuditMetadata(plan, "qa_verdict_id", verdict.ID)
	case domain.StateQualityGate:
		decision, err := g.evaluateTx(ctx, q, run, cmd.ExpectedLeaseEpoch)
		if err != nil {
			return err
		}
		if decision.Decision != "pass" {
			return domain.ErrEvidenceInvalid
		}
		return addAuditMetadata(plan, "gate_decision_id", decision.ID)
	default:
		return domain.ErrStageNotOperational
	}
	return nil
}
func addAuditMetadata(plan *application.TransitionPlan, key, value string) error {
	metadata := map[string]any{}
	if len(plan.Audit.Metadata) > 0 {
		if err := json.Unmarshal(plan.Audit.Metadata, &metadata); err != nil {
			return fmt.Errorf("audit metadata: %w", err)
		}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[key] = value
	encoded, err := jsonBytes(metadata)
	if err != nil {
		return err
	}
	plan.Audit.Metadata = encoded
	return nil
}
func (g EvidenceGuards) evaluateTx(ctx context.Context, q *db.Queries, run db.TaskRun, epoch int64) (gate.Decision, error) {
	head, err := g.resolveHead(ctx, q, run)
	if err != nil {
		return gate.Decision{}, err
	}
	ev, err := loadEvidence(ctx, q, run.RunID, run.CommitSha.String)
	if err != nil {
		return gate.Decision{}, err
	}
	verdict, err := loadVerdict(ctx, q, run.RunID, run.CommitSha.String)
	if err != nil {
		return gate.Decision{}, err
	}
	decision := gate.Evaluate(gate.Input{Run: snapshotFromTaskRun(run), HeadSHA: head, Evidence: ev, Verdict: verdict, Now: g.now(), ExpectedLeaseEpoch: epoch})
	conditions, err := jsonBytes(decision.Conditions)
	if err != nil {
		return decision, err
	}
	row, err := q.InsertGateDecision(ctx, db.InsertGateDecisionParams{RunID: run.RunID, TaskID: run.TaskID, Attempt: run.Attempt, SubjectCommitSha: run.CommitSha.String, VerifiedHeadSha: textFromOptional(head), PolicyName: decision.PolicyName, PolicyVersion: decision.PolicyVersion, Decision: decision.Decision, Conditions: conditions, BlockingReasons: decision.BlockingReasons, Evaluator: "gate-engine", CorrelationID: run.CorrelationID, EvaluatedAt: timestamptzFrom(&decision.EvaluatedAt)})
	if err != nil {
		return decision, err
	}
	decision.ID = uuidToString(row.DecisionID)
	if err = insertEvidenceEvent(ctx, q, run, "quality_gate.decision.v1", map[string]any{"decision_id": decision.ID, "attempt": run.Attempt, "subject_commit_sha": decision.SubjectCommitSHA, "verified_head_sha": head, "policy_name": decision.PolicyName, "policy_version": decision.PolicyVersion, "decision": decision.Decision, "conditions": decision.Conditions, "blocking_reasons": decision.BlockingReasons, "lease_epoch": decision.LeaseEpoch}, decision.EvaluatedAt); err != nil {
		return decision, err
	}
	return decision, nil
}
