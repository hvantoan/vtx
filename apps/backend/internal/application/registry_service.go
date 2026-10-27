package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vtx/platform/internal/domain"
)

// VTX-3 outbound contract types (§3, documented in Phase 6).
const (
	OutboxEventTaskRunCreated        = "task_run.created.v1"
	OutboxEventTaskRunStateChanged   = "task_run.state_changed.v1"
	OutboxEventTaskRunIntentDeferred = "task_run.intent_deferred.v1"
)

// Audit action vocabulary: real transitions use their domain action name; run
// creation is its own verb.
const AuditActionCreated = domain.Action("created")

// defaultRetryDelay is the schedule_retry due date. ponytail: fixed delay — the
// budgeted, jittered delay is VTX-5's scheduler decision, not the registry's.
const defaultRetryDelay = time.Minute

// Reasons are sanitized before they are stored or logged: caller-supplied text
// must never smuggle newlines or unbounded length into an audit row.
const maxReasonLength = 500

// RegistryService is the only mutation path for a task run (spec §4).
type RegistryService struct {
	store        RegistryStore
	consumer     string
	clock        Clock
	logger       *slog.Logger
	retryPlanner RetryPlanner
}

func NewRegistryService(store RegistryStore, consumer string, clock Clock, logger *slog.Logger) *RegistryService {
	if clock == nil {
		clock = SystemClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RegistryService{store: store, consumer: consumer, clock: clock, logger: logger}
}

// TransitionRun evaluates one transition and persists it. Errors are domain
// typed errors: not_found, stale_version, stale_lease, already_terminal,
// invalid_transition, forbidden, stage_not_operational.
func (s *RegistryService) WithRetryPlanner(p RetryPlanner) *RegistryService {
	s.retryPlanner = p
	return s
}

func (s *RegistryService) TransitionRun(ctx context.Context, cmd TransitionCommand) (RunSnapshot, error) {
	if err := cmd.validate(); err != nil {
		return RunSnapshot{}, err
	}
	now := s.clock.Now()

	run, err := s.store.TransitionRun(ctx, cmd, func(locked RunSnapshot) (TransitionPlan, error) {
		if s.retryPlanner != nil && (cmd.Action == domain.ActionScheduleRetry || cmd.FailureClass != "") {
			var err error
			cmd, err = s.retryPlanner.PlanFailure(locked, cmd, now)
			if err != nil {
				return TransitionPlan{}, err
			}
		}
		evaluate := domain.Evaluate
		if guarded, ok := s.store.(GuardedRegistryStore); ok && guarded.HasTransitionGuard(locked.State, cmd.Action) {
			evaluate = domain.EvaluateWithGuard
		}
		decision, err := evaluate(domain.Request{
			State:              locked.State,
			PreparationReady:   locked.PreparationReady,
			Version:            locked.Version,
			ExpectedVersion:    cmd.ExpectedVersion,
			LeaseOwner:         locked.LeaseOwner,
			LeaseEpoch:         locked.LeaseEpoch,
			ExpectedLeaseEpoch: cmd.ExpectedLeaseEpoch,
			LeaseExpiresAt:     locked.LeaseExpiresAt,
			ResumeState:        locked.ResumeState,
			RetryDueAt:         locked.RetryDueAt,
			Action:             cmd.Action,
			Actor:              cmd.Actor,
			ActorType:          cmd.ActorType,
		}, now)
		if err != nil {
			return TransitionPlan{}, err
		}
		return buildTransitionPlan(locked, cmd, decision, now)
	})
	if err != nil {
		if isRunNotFound(err) {
			return RunSnapshot{}, fmt.Errorf("%w: run %s", domain.ErrNotFound, cmd.RunID)
		}
		return RunSnapshot{}, err
	}

	s.logger.Info("registry transition applied",
		"metric", "registry_transition_success",
		"intent_id", run.IntentID,
		"run_id", run.RunID,
		"task_id", run.TaskID,
		"action", string(cmd.Action),
		"actor", string(cmd.Actor),
		"actor_type", string(cmd.ActorType),
		"state", string(run.State),
		"version", run.Version,
		"correlation_id", run.CorrelationID,
	)
	return run, nil
}

// ConsumeIntent applies one execution_intent.created.v1 delivery. The receipt
// makes replays a no-op; the run is admitted only when the task has no active
// run, otherwise the intent lands in the durable FIFO wait queue.
func (s *RegistryService) ConsumeIntent(ctx context.Context, payload []byte) (ConsumeOutcome, error) {
	evt, err := ParseIntentEvent(payload)
	if err != nil {
		return ConsumeOutcome{}, err
	}
	projectKey, err := projectKeyFromIssueKey(evt.IssueKey)
	if err != nil {
		return ConsumeOutcome{}, err
	}
	now := s.clock.Now()

	plan := ConsumePlan{
		Consumer: s.consumer,
		Task: RegistryTaskIdentity{
			JiraSiteID:   evt.JiraSiteID,
			JiraIssueID:  evt.IssueID,
			JiraIssueKey: evt.IssueKey,
			ProjectKey:   projectKey,
		},
	}
	plan.OnAdmit = ConsumeBranch{
		Audit: &AuditRecord{
			PreviousState: "",
			CurrentState:  domain.StateQueued,
			Action:        AuditActionCreated,
			Actor:         domain.Actor(s.consumer),
			ActorType:     domain.ActorTypeSystem,
			CorrelationID: evt.CorrelationID,
			Version:       0, // I-05: creation audit sits at the run's version 0
		},
		Outbound: func(run RunSnapshot) (OutboundEvent, error) {
			return jsonEvent(OutboxEventTaskRunCreated, map[string]any{
				"run_id":         run.RunID,
				"task_id":        run.TaskID,
				"intent_id":      run.IntentID,
				"jira_site_id":   evt.JiraSiteID,
				"jira_issue_id":  evt.IssueID,
				"issue_key":      evt.IssueKey,
				"state":          string(domain.StateQueued),
				"version":        run.Version,
				"correlation_id": evt.CorrelationID,
				"occurred_at":    now.Format(time.RFC3339),
			})
		},
	}
	plan.OnDefer = ConsumeBranch{
		Reason: "active_run_in_progress",
		Outbound: func(task RunSnapshot) (OutboundEvent, error) {
			return jsonEvent(OutboxEventTaskRunIntentDeferred, map[string]any{
				"intent_id":     evt.IntentID,
				"task_id":       task.TaskID,
				"jira_site_id":  evt.JiraSiteID,
				"jira_issue_id": evt.IssueID,
				"issue_key":     evt.IssueKey,
				"disposition":   "pending_deferred",
				"reason":        "active_run_in_progress",
				"received_at":   now.Format(time.RFC3339),
			})
		},
	}

	outcome, err := s.store.ConsumeIntent(ctx, plan, evt)
	if err != nil {
		return ConsumeOutcome{}, err
	}

	s.logger.Info("registry intent consumed",
		"metric", "registry_run_admitted",
		"intent_id", evt.IntentID,
		"event_id", evt.EventID,
		"task_id", outcome.TaskID,
		"run_id", outcome.RunID,
		"duplicate", outcome.Duplicate,
		"deferred", outcome.Deferred,
		"consumer", s.consumer,
		"correlation_id", evt.CorrelationID,
	)
	return outcome, nil
}

func buildTransitionPlan(locked RunSnapshot, cmd TransitionCommand, decision domain.Decision, now time.Time) (TransitionPlan, error) {
	update := RunUpdate{
		State:       decision.To,
		ResumeState: decision.ResumeState,
		Attempt:     locked.Attempt,
	}
	reason := sanitizeReason(cmd.Reason)
	if cmd.FailureClass != "" {
		update.FailureClass = &cmd.FailureClass
		update.RetryPolicyVersion = &cmd.RetryPolicyVersion
	}

	switch cmd.Action {
	case domain.ActionBegin:
		started := now
		update.StartedAt = &started
	case domain.ActionScheduleRetry:
		due := now.Add(defaultRetryDelay)
		if cmd.RetryDueAt != nil {
			due = *cmd.RetryDueAt
		}
		update.RetryDueAt = &due
		update.Attempt = locked.Attempt + 1
	case domain.ActionFail:
		code := "run_failed_by_policy"
		if cmd.ErrorCode != "" {
			code = cmd.ErrorCode
		}
		update.ErrorCode = &code
		if reason != "" {
			update.ErrorMessage = &reason
		}
	}

	if decision.To.IsTerminal() {
		finished := now
		update.FinishedAt = &finished
	}

	// task_run.state_changed.v1 — every field is derived from the locked row or
	// the audit record; nothing is inferred.
	payload, err := jsonEvent(OutboxEventTaskRunStateChanged, map[string]any{
		"run_id":         locked.RunID,
		"task_id":        locked.TaskID,
		"intent_id":      locked.IntentID,
		"previous_state": string(decision.From),
		"current_state":  string(decision.To),
		"action":         string(decision.Action),
		"actor":          string(decision.Actor),
		"actor_type":     string(decision.ActorType),
		"version":        decision.Version,
		"correlation_id": locked.CorrelationID,
		"occurred_at":    now.Format(time.RFC3339),
	})
	if err != nil {
		return TransitionPlan{}, err
	}

	return TransitionPlan{
		Update: update,
		Audit: AuditRecord{
			PreviousState: decision.From,
			CurrentState:  decision.To,
			Action:        decision.Action,
			Actor:         decision.Actor,
			ActorType:     decision.ActorType,
			CorrelationID: locked.CorrelationID,
			Version:       decision.Version,
			Metadata:      cmd.Metadata,
		},
		Outbound: payload,
	}, nil
}

func jsonEvent(eventType string, body map[string]any) (OutboundEvent, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return OutboundEvent{}, fmt.Errorf("marshal %s: %w", eventType, err)
	}
	return OutboundEvent{EventType: eventType, Payload: encoded}, nil
}

// sanitizeReason flattens whitespace and bounds the length so a caller cannot
// inject structure into an audit row or a log line.
func sanitizeReason(reason string) string {
	flat := strings.Join(strings.Fields(reason), " ")
	if len(flat) > maxReasonLength {
		flat = flat[:maxReasonLength]
	}
	return flat
}

// projectKeyFromIssueKey extracts the Jira project key ("VTX-3" → "VTX"). An
// issue key without a project prefix is a contract violation, not a guess.
func projectKeyFromIssueKey(issueKey string) (string, error) {
	key, _, found := strings.Cut(issueKey, "-")
	if !found || key == "" {
		return "", fmt.Errorf("%w: issue_key %q has no project prefix", ErrPayloadInvalid, issueKey)
	}
	return key, nil
}

func (cmd TransitionCommand) validate() error {
	if cmd.RecoverExpiredLease && (cmd.Actor != domain.ActorPolicy || cmd.ActorType != domain.ActorTypeSystem) {
		return domain.ErrForbidden
	}
	if cmd.RunID == "" {
		return fmt.Errorf("%w: run_id is required", domain.ErrNotFound)
	}
	if !cmd.Action.Valid() {
		return fmt.Errorf("%w: unknown action %q", domain.ErrInvalidTransition, cmd.Action)
	}
	if !cmd.Actor.Valid() {
		return fmt.Errorf("%w: unknown actor %q", domain.ErrForbidden, cmd.Actor)
	}
	return nil
}

func isRunNotFound(err error) bool { return errors.Is(err, ErrRunNotFound) }

// MergeRetryMetadata preserves caller context while policy-owned keys win.
func MergeRetryMetadata(raw []byte, values map[string]any) ([]byte, error) {
	metadata := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, fmt.Errorf("retry metadata: %w", err)
		}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	for key, value := range values {
		metadata[key] = value
	}
	return json.Marshal(metadata)
}
