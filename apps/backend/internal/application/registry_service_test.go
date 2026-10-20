package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
)

// fakeStore records what the service asked to persist. It deliberately does no
// transaction work: the DB behaviour lives in the integration suite, while
// these tests pin the rules and the contract.
type fakeStore struct {
	snapshot application.RunSnapshot
	notFound bool

	plans        []application.TransitionPlan
	commands     []application.TransitionCommand
	consumePlan  *application.ConsumePlan
	consumeEvent *application.IntentEvent
	consumeErr   error
}

func (f *fakeStore) TransitionRun(_ context.Context, cmd application.TransitionCommand, decide func(application.RunSnapshot) (application.TransitionPlan, error)) (application.RunSnapshot, error) {
	if f.notFound {
		return application.RunSnapshot{}, application.ErrRunNotFound
	}
	f.commands = append(f.commands, cmd)
	plan, err := decide(f.snapshot)
	if err != nil {
		return application.RunSnapshot{}, err
	}
	f.plans = append(f.plans, plan)
	out := f.snapshot
	out.State = plan.Update.State
	out.Version = plan.Audit.Version
	return out, nil
}

func (f *fakeStore) ConsumeIntent(_ context.Context, plan application.ConsumePlan, evt application.IntentEvent) (application.ConsumeOutcome, error) {
	if f.consumeErr != nil {
		return application.ConsumeOutcome{}, f.consumeErr
	}
	f.consumePlan = &plan
	f.consumeEvent = &evt
	outbound, err := plan.OnAdmit.Outbound(application.RunSnapshot{RunID: "run-1"})
	if err != nil {
		return application.ConsumeOutcome{}, err
	}
	return application.ConsumeOutcome{TaskID: "task-1", RunID: "run-1", OutboundType: outbound.EventType}, nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func testService(store application.RegistryStore, now time.Time) *application.RegistryService {
	return application.NewRegistryService(store, "registry-intake", fixedClock{t: now}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func validIntentPayload(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	body := map[string]any{
		"event_id":         "11111111-1111-1111-1111-111111111111",
		"intent_id":        "22222222-2222-2222-2222-222222222222",
		"issue_id":         "10001",
		"issue_key":        "VTX-3",
		"jira_site_id":     "vtxsolution.atlassian.net",
		"changelog_id":     "44444",
		"target_status_id": "10007",
		"correlation_id":   "corr-1",
		"occurred_at":      "2026-10-19T06:30:00Z",
	}
	if mutate != nil {
		mutate(body)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

func TestParseIntentEventRequiresEveryContractField(t *testing.T) {
	if _, err := application.ParseIntentEvent(validIntentPayload(t, nil)); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}

	// §2: a missing field is a schema/version error — never a substituted default.
	for _, field := range []string{"event_id", "intent_id", "issue_id", "issue_key", "jira_site_id", "changelog_id", "target_status_id", "correlation_id", "occurred_at"} {
		payload := validIntentPayload(t, func(body map[string]any) { delete(body, field) })
		_, err := application.ParseIntentEvent(payload)
		if !errors.Is(err, application.ErrPayloadInvalid) {
			t.Errorf("missing %s: got %v, want ErrPayloadInvalid", field, err)
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("missing %s: error does not name the field: %v", field, err)
		}
	}

	for _, tc := range []struct {
		name  string
		input []byte
	}{
		{"not json", []byte("{oops")},
		{"bad timestamp", validIntentPayload(t, func(body map[string]any) { body["occurred_at"] = "yesterday" })},
	} {
		if _, err := application.ParseIntentEvent(tc.input); !errors.Is(err, application.ErrPayloadInvalid) {
			t.Errorf("%s: got %v, want ErrPayloadInvalid", tc.name, err)
		}
	}
}

func TestTransitionRunTranslatesMissingRow(t *testing.T) {
	svc := testService(&fakeStore{notFound: true}, time.Now())
	_, err := svc.TransitionRun(context.Background(), application.TransitionCommand{
		RunID: "33333333-3333-3333-3333-333333333333", Action: domain.ActionCancel, Actor: domain.ActorOperator,
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v, want domain.ErrNotFound", err)
	}
}

func TestTransitionRunWritesNothingWhenTheDomainRejects(t *testing.T) {
	now := time.Date(2026, 10, 19, 6, 30, 0, 0, time.UTC)
	store := &fakeStore{snapshot: application.RunSnapshot{
		RunID: "run-1", TaskID: "task-1", IntentID: "intent-1", State: domain.StatePRDraft, Version: 7,
		LeaseOwner: new("orchestrator-1"), LeaseEpoch: 1, LeaseExpiresAt: new(now.Add(time.Minute)),
	}}
	svc := testService(store, now)

	// A non-operational hop: valid contract, live lease, right actor — and it
	// still must not run (gate G4).
	_, err := svc.TransitionRun(context.Background(), application.TransitionCommand{
		RunID: "run-1", ExpectedVersion: 7, ExpectedLeaseEpoch: 1,
		Action: domain.ActionComplete, Actor: domain.ActorOrchestrator,
	})
	if !errors.Is(err, domain.ErrStageNotOperational) {
		t.Fatalf("got %v, want ErrStageNotOperational", err)
	}
	if len(store.plans) != 0 {
		t.Fatalf("rejected transition still produced %d plan(s) — audit/outbox would be forged", len(store.plans))
	}
}

func TestTransitionRunPlanCarriesAuditAndOutbound(t *testing.T) {
	now := time.Date(2026, 10, 19, 6, 30, 0, 0, time.UTC)
	lease := "scheduler-1"
	expires := now.Add(time.Minute)
	store := &fakeStore{snapshot: application.RunSnapshot{
		RunID: "run-1", TaskID: "task-1", IntentID: "intent-1",
		State: domain.StateCoding, Version: 4, Attempt: 2, CorrelationID: "corr-1",
		LeaseOwner: &lease, LeaseEpoch: 1, LeaseExpiresAt: &expires,
	}}
	svc := testService(store, now)

	run, err := svc.TransitionRun(context.Background(), application.TransitionCommand{
		RunID: "run-1", ExpectedVersion: 4, ExpectedLeaseEpoch: 1,
		Action: domain.ActionScheduleRetry, Actor: domain.ActorPolicy, Reason: "  budget\n  exhausted  ",
	})
	if err != nil {
		t.Fatalf("TransitionRun: %v", err)
	}
	if run.State != domain.StateRetryWait || run.Version != 5 {
		t.Fatalf("run = %s v%d, want retry_wait v5", run.State, run.Version)
	}

	plan := store.plans[0]
	if plan.Audit.Version != 5 || plan.Audit.PreviousState != domain.StateCoding || plan.Audit.CurrentState != domain.StateRetryWait {
		t.Errorf("audit = %+v, want coding→retry_wait at version 5", plan.Audit)
	}
	if plan.Update.Attempt != 3 {
		t.Errorf("attempt = %d, want 3 (one retry consumed)", plan.Update.Attempt)
	}
	if plan.Update.RetryDueAt == nil || !plan.Update.RetryDueAt.Equal(now.Add(time.Minute)) {
		t.Errorf("retry_due_at = %v, want %v", plan.Update.RetryDueAt, now.Add(time.Minute))
	}
	if plan.Update.ResumeState == nil || *plan.Update.ResumeState != domain.StateCoding {
		t.Errorf("resume_state = %v, want coding", plan.Update.ResumeState)
	}
	if plan.Outbound.EventType != application.OutboxEventTaskRunStateChanged {
		t.Errorf("outbound = %s, want %s", plan.Outbound.EventType, application.OutboxEventTaskRunStateChanged)
	}
	assertPayloadField(t, plan.Outbound.Payload, "current_state", "retry_wait")
	assertPayloadField(t, plan.Outbound.Payload, "previous_state", "coding")
	assertPayloadField(t, plan.Outbound.Payload, "intent_id", "intent-1")
}

func TestFailActionSanitizesReason(t *testing.T) {
	lease := "orchestrator-1"
	expires := time.Now().Add(time.Minute)
	store := &fakeStore{snapshot: application.RunSnapshot{
		RunID: "run-1", TaskID: "task-1", IntentID: "intent-1", State: domain.StateQA, Version: 1,
		LeaseOwner: &lease, LeaseEpoch: 1, LeaseExpiresAt: &expires,
	}}
	svc := testService(store, time.Now())

	long := strings.Repeat("x", 1200)
	if _, err := svc.TransitionRun(context.Background(), application.TransitionCommand{
		RunID: "run-1", ExpectedVersion: 1, ExpectedLeaseEpoch: 1,
		Action: domain.ActionFail, Actor: domain.ActorPolicy, Reason: "failed\nbecause   " + long,
	}); err != nil {
		t.Fatalf("TransitionRun: %v", err)
	}
	plan := store.plans[0]
	if plan.Update.ErrorCode == nil || *plan.Update.ErrorCode != "run_failed_by_policy" {
		t.Errorf("error_code = %v, want run_failed_by_policy", plan.Update.ErrorCode)
	}
	if plan.Update.ErrorMessage == nil {
		t.Fatal("error_message missing")
	}
	if strings.ContainsAny(*plan.Update.ErrorMessage, "\n\r\t") {
		t.Errorf("error_message still has control characters: %q", *plan.Update.ErrorMessage)
	}
	if len(*plan.Update.ErrorMessage) > 500 {
		t.Errorf("error_message length = %d, want <= 500", len(*plan.Update.ErrorMessage))
	}
	if plan.Update.FinishedAt == nil {
		t.Errorf("terminal transition must set finished_at")
	}
}

func TestTransitionRunRejectsUnknownActionBeforeTouchingStorage(t *testing.T) {
	store := &fakeStore{}
	svc := testService(store, time.Now())
	_, err := svc.TransitionRun(context.Background(), application.TransitionCommand{
		RunID: "run-1", ExpectedVersion: 1, Action: domain.Action("teleport"), Actor: domain.ActorPolicy,
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("got %v, want ErrInvalidTransition", err)
	}
	if len(store.commands) != 0 {
		t.Errorf("store was called %d time(s) before validation", len(store.commands))
	}
}

func TestConsumeIntentBuildsAdmitAndDeferBranches(t *testing.T) {
	store := &fakeStore{}
	svc := testService(store, time.Date(2026, 10, 19, 6, 30, 0, 0, time.UTC))

	outcome, err := svc.ConsumeIntent(context.Background(), validIntentPayload(t, nil))
	if err != nil {
		t.Fatalf("ConsumeIntent: %v", err)
	}
	if outcome.RunID != "run-1" || outcome.Deferred {
		t.Errorf("outcome = %+v, want an admitted run", outcome)
	}
	if store.consumePlan == nil {
		t.Fatal("plan was not forwarded to the store")
	}
	plan := store.consumePlan
	if plan.Consumer != "registry-intake" {
		t.Errorf("consumer = %q, want registry-intake", plan.Consumer)
	}
	if plan.Task.ProjectKey != "VTX" || plan.Task.JiraIssueID != "10001" || plan.Task.JiraSiteID != "vtxsolution.atlassian.net" {
		t.Errorf("task identity = %+v, want VTX/10001/vtxsolution.atlassian.net", plan.Task)
	}
	if plan.OnAdmit.Audit == nil || plan.OnAdmit.Audit.Version != 0 {
		t.Errorf("admit audit = %+v, want a version-0 creation row (I-05)", plan.OnAdmit.Audit)
	}
	if plan.OnDefer.Reason == "" || plan.OnDefer.Outbound == nil {
		t.Errorf("defer branch incomplete: %+v", plan.OnDefer)
	}

	created := plan.OnAdmit.Outbound
	event, err := created(application.RunSnapshot{RunID: "run-9", TaskID: "task-9", IntentID: "intent-9", Version: 0})
	if err != nil {
		t.Fatalf("build created event: %v", err)
	}
	if event.EventType != application.OutboxEventTaskRunCreated {
		t.Errorf("event type = %s, want %s", event.EventType, application.OutboxEventTaskRunCreated)
	}
	assertPayloadField(t, event.Payload, "state", "queued")
	assertPayloadField(t, event.Payload, "run_id", "run-9")
	assertPayloadField(t, event.Payload, "intent_id", "intent-9")

	deferred, err := plan.OnDefer.Outbound(application.RunSnapshot{})
	if err != nil {
		t.Fatalf("build deferred event: %v", err)
	}
	assertPayloadField(t, deferred.Payload, "disposition", "pending_deferred")
	assertPayloadField(t, deferred.Payload, "intent_id", "22222222-2222-2222-2222-222222222222")
}

func TestConsumeIntentRejectsIssueKeyWithoutProject(t *testing.T) {
	store := &fakeStore{}
	svc := testService(store, time.Now())
	_, err := svc.ConsumeIntent(context.Background(), validIntentPayload(t, func(body map[string]any) {
		body["issue_key"] = "VTX3"
	}))
	if !errors.Is(err, application.ErrPayloadInvalid) {
		t.Fatalf("got %v, want ErrPayloadInvalid", err)
	}
	if store.consumePlan != nil {
		t.Errorf("store was called for an invalid payload")
	}
}

func assertPayloadField(t *testing.T, payload []byte, key, want string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if got, _ := body[key].(string); got != want {
		t.Errorf("payload[%s] = %v, want %q", key, body[key], want)
	}
}
