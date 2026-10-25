package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vtx/platform/internal/domain"
)

// VTX-3 registry ports. Each port has exactly one production implementation
// (pgx) and one fake in tests — no factories, no plugin registry.

// Clock is injected so lease expiry and retry scheduling are testable without
// sleeping. The zero value is not usable; use SystemClock.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Store-level sentinels. They are deliberately distinct from the domain errors
// so the service can translate one into the other and tests can tell "the row
// was missing" from "the adapter failed".
var (
	ErrRunNotFound = errors.New("registry store: run not found")
	// ErrPayloadInvalid marks a poison event: a required contract field is
	// missing or malformed. The dispatcher quarantines it instead of retrying
	// forever (§0.E), so it must never be wrapped around a transient failure.
	ErrPayloadInvalid = errors.New("registry: event payload invalid")
)

// RunSnapshot is a task_runs row as the domain needs it.
type RunSnapshot struct {
	RunID              string
	TaskID             string
	IntentID           string
	State              domain.State
	Version            int64
	Attempt            int32
	ResumeState        *domain.State
	RetryDueAt         *time.Time
	LeaseOwner         *string
	LeaseEpoch         int64
	LeaseExpiresAt     *time.Time
	CorrelationID      string
	CommitSHA          *string
	FailureClass       string
	RetryPolicyVersion string
	StartedAt          *time.Time
	FinishedAt         *time.Time
}

// TransitionCommand is one caller's transition request.
type TransitionCommand struct {
	RunID                                       string
	ExpectedVersion                             int64
	ExpectedLeaseEpoch                          int64
	Action                                      domain.Action
	Actor                                       domain.Actor
	ActorType                                   domain.ActorType
	Reason                                      string
	Metadata                                    []byte
	RetryDueAt                                  *time.Time
	FailureClass, RetryPolicyVersion, ErrorCode string
	// RecoverExpiredLease is internal-only: policy/system may fence a dead worker.
	RecoverExpiredLease bool
}

// RunUpdate is the mutable part of a run a transition writes.
type RunUpdate struct {
	State                            domain.State
	ResumeState                      *domain.State
	RetryDueAt                       *time.Time
	Attempt                          int32
	ErrorCode                        *string
	ErrorMessage                     *string
	StartedAt                        *time.Time
	FinishedAt                       *time.Time
	FailureClass, RetryPolicyVersion *string
}

// AuditRecord is one task_run_events row.
type AuditRecord struct {
	PreviousState domain.State
	CurrentState  domain.State
	Action        domain.Action
	Actor         domain.Actor
	ActorType     domain.ActorType
	CorrelationID string
	Version       int64
	Metadata      []byte
}

// OutboundEvent is one outbox row the registry publishes. Every event carries
// the run's intent_id: outbox_events.intent_id is NOT NULL (VTX-2).
type OutboundEvent struct {
	EventType string
	Payload   []byte
}

// TransitionPlan is a fully decided transition, ready to persist atomically.
// The service owns the contract and the rules; the store owns the DB.
type TransitionPlan struct {
	Update   RunUpdate
	Audit    AuditRecord
	Outbound OutboundEvent
}

// ConsumeBranch is one of the two outcomes of consuming an intent.
type ConsumeBranch struct {
	Audit *AuditRecord
	// Outbound builds the event payload: the admit branch needs the run id the
	// database generated, so the builder runs after the insert, inside the same
	// transaction.
	Outbound func(run RunSnapshot) (OutboundEvent, error)
	Reason   string
}

// ConsumePlan is a fully validated intent consumption.
type ConsumePlan struct {
	Consumer string
	Task     RegistryTaskIdentity
	OnAdmit  ConsumeBranch
	OnDefer  ConsumeBranch
}

// RegistryTaskIdentity is the registry_tasks row an intent resolves to.
type RegistryTaskIdentity struct {
	JiraSiteID   string
	JiraIssueID  string
	JiraIssueKey string
	ProjectKey   string
}

// RegistryStore is the transactional persistence surface. Both methods own
// their transaction: the registry's correctness claims (I-07, TR-17) are
// statements about a single transaction, so the boundary lives here.
type RegistryStore interface {
	// TransitionRun locks the run row, invokes decide with the locked snapshot
	// (so error classification sees the freshest version/lease), then applies
	// the plan: CAS update + audit + outbound, all-or-nothing.
	TransitionRun(ctx context.Context, cmd TransitionCommand, decide func(RunSnapshot) (TransitionPlan, error)) (RunSnapshot, error)

	// ConsumeIntent inserts the receipt, upserts the task, admits or defers the
	// run, and appends the audit + outbound rows — in one transaction. A
	// duplicate receipt returns ConsumeOutcome{Duplicate: true} and writes
	// nothing else.
	ConsumeIntent(ctx context.Context, plan ConsumePlan, evt IntentEvent) (ConsumeOutcome, error)
}

// RunView is a read model of a task_runs row for the HTTP API.
type RunView struct {
	RunID          string
	TaskID         string
	State          domain.State
	Version        int64
	Attempt        int32
	ResumeState    *domain.State
	RetryDueAt     *time.Time
	CommitSHA      *string
	Branch         *string
	ErrorCode      *string
	ErrorMessage   *string
	LeaseOwner     *string
	LeaseEpoch     int64
	LeaseExpiresAt *time.Time
	CorrelationID  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

// TaskView is a read model of a registry_tasks row.
type TaskView struct {
	TaskID       string
	JiraSiteID   string
	JiraIssueID  string
	JiraIssueKey string
	ProjectKey   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// EventView is a read model of an audit row.
type EventView struct {
	PreviousState domain.State
	CurrentState  domain.State
	Action        domain.Action
	Actor         domain.Actor
	ActorType     domain.ActorType
	Version       int64
	Metadata      []byte
	CreatedAt     time.Time
}

// RegistryReader is the read-only surface (Phase 5). Separate from
// RegistryStore because reads never participate in a mutation transaction.
type RegistryReader interface {
	GetTask(ctx context.Context, siteID, issueID string) (TaskView, error)
	GetRun(ctx context.Context, runID string) (RunView, error)
	ListEvents(ctx context.Context, runID string, afterVersion *int64, limit int32) ([]EventView, error)
}

// ErrReadNotFound is returned when a read target does not exist.
var ErrReadNotFound = errors.New("registry: read target not found")

// IntentEvent is the validated payload of execution_intent.created.v1 (§2).
// Field names/types mirror the VTX-2 contract; the identity fields are
// required, never defaulted.
type IntentEvent struct {
	EventID        string
	IntentID       string
	IssueID        string
	IssueKey       string
	JiraSiteID     string
	ChangelogID    string
	TargetStatusID string
	CorrelationID  string
	OccurredAt     time.Time
}

// ConsumeOutcome reports what the consumption did.
type ConsumeOutcome struct {
	TaskID       string
	RunID        string
	Deferred     bool
	Duplicate    bool
	OutboundType string
}

// ParseIntentEvent validates the raw outbox payload. A missing or malformed
// required field is a schema/version error — the service never substitutes a
// default (§2), and the caller quarantines the event.
func ParseIntentEvent(payload []byte) (IntentEvent, error) {
	// One definition of the wire contract: the producer type is reused so a
	// field added upstream cannot silently disappear from validation.
	var raw ExecutionIntentCreatedV1
	if err := json.Unmarshal(payload, &raw); err != nil {
		return IntentEvent{}, fmt.Errorf("%w: not JSON: %v", ErrPayloadInvalid, err)
	}

	// Ordered, not a map: the first missing field must be deterministic so
	// dead-letter reasons are stable for operators and redrive tests.
	required := []struct{ field, value string }{
		{"event_id", raw.EventID},
		{"intent_id", raw.IntentID},
		{"issue_id", raw.IssueID},
		{"issue_key", raw.IssueKey},
		{"jira_site_id", raw.JiraSiteID},
		{"changelog_id", raw.ChangelogID},
		{"target_status_id", raw.TargetStatusID},
		{"correlation_id", raw.CorrelationID},
		{"occurred_at", raw.OccurredAt},
	}
	for _, req := range required {
		if req.value == "" {
			return IntentEvent{}, fmt.Errorf("%w: missing %s", ErrPayloadInvalid, req.field)
		}
	}

	occurredAt, err := time.Parse(time.RFC3339, raw.OccurredAt)
	if err != nil {
		return IntentEvent{}, fmt.Errorf("%w: occurred_at %q is not RFC3339", ErrPayloadInvalid, raw.OccurredAt)
	}

	return IntentEvent{
		EventID:        raw.EventID,
		IntentID:       raw.IntentID,
		IssueID:        raw.IssueID,
		IssueKey:       raw.IssueKey,
		JiraSiteID:     raw.JiraSiteID,
		ChangelogID:    raw.ChangelogID,
		TargetStatusID: raw.TargetStatusID,
		CorrelationID:  raw.CorrelationID,
		OccurredAt:     occurredAt.UTC(),
	}, nil
}

// GuardedRegistryStore advertises only hops enforced inside its write tx.
// The registry uses EvaluateWithGuard for these; a plain store stays closed.
type GuardedRegistryStore interface {
	HasTransitionGuard(domain.State, domain.Action) bool
}

// RetryPlanner selects a bounded action on the locked snapshot.
type RetryPlanner interface {
	PlanFailure(RunSnapshot, TransitionCommand, time.Time) (TransitionCommand, error)
}
