package domain

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// TestStatesMatchSpecLiterals locks the wire contract (§0.C machine values).
func TestStatesMatchSpecLiterals(t *testing.T) {
	want := []State{
		"queued", "preparing", "coding", "verifying", "qa", "quality_gate",
		"pr_draft", "completed", "blocked", "retry_wait", "failed", "cancelled",
	}
	if len(want) != 12 {
		t.Fatalf("spec has 12 states, test lists %d", len(want))
	}
	for _, s := range want {
		if !s.Valid() {
			t.Errorf("state %q is not accepted by the machine", s)
		}
	}
	if State("done").Valid() {
		t.Errorf("unknown state was accepted")
	}
	for _, terminal := range []State{StateCompleted, StateFailed, StateCancelled} {
		if !terminal.IsTerminal() || terminal.IsNonterminal() {
			t.Errorf("%s must be terminal", terminal)
		}
	}
	for _, nonterminal := range []State{StateQueued, StateBlocked, StateRetryWait, StateQA} {
		if nonterminal.IsTerminal() || !nonterminal.IsNonterminal() {
			t.Errorf("%s must be nonterminal", nonterminal)
		}
	}
}

// TestValidTransitions walks every hop of the §0.C table.
func TestValidTransitions(t *testing.T) {
	held := Request{
		LeaseOwner: new("scheduler-1"), LeaseEpoch: 3, ExpectedLeaseEpoch: 3,
		LeaseExpiresAt: new(now.Add(time.Minute)), ActorType: ActorTypeSystem,
	}

	tests := []struct {
		name       string
		req        Request
		wantState  State
		wantResume *State
	}{
		{"queued begin", withState(held, StateQueued, ActionBegin, ActorScheduler), StatePreparing, nil},
		{"block from queued", withState(Request{}, StateQueued, ActionBlock, ActorOperator), StateBlocked, new(StateQueued)},
		{"schedule_retry from coding", withState(held, StateCoding, ActionScheduleRetry, ActorPolicy), StateRetryWait, new(StateCoding)},
		{"blocked resume", withState(Request{ResumeState: new(StateQA)}, StateBlocked, ActionResume, ActorOperator), StateQA, nil},
		{"retry_wait retry_due", withState(Request{ResumeState: new(StateCoding), RetryDueAt: new(now.Add(-time.Minute)), LeaseOwner: new("sched"), LeaseEpoch: 1, ExpectedLeaseEpoch: 1, LeaseExpiresAt: new(now.Add(time.Minute))}, StateRetryWait, ActionRetryDue, ActorScheduler), StateCoding, nil},
		{"cancel from verifying", withState(held, StateVerifying, ActionCancel, ActorOperator), StateCancelled, nil},
		{"fail from retry_wait", withState(held, StateRetryWait, ActionFail, ActorPolicy), StateFailed, nil},
		{"block from quality_gate", withState(held, StateQualityGate, ActionBlock, ActorPolicy), StateBlocked, new(StateQualityGate)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := Evaluate(tc.req, now)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if decision.To != tc.wantState {
				t.Errorf("to = %s, want %s", decision.To, tc.wantState)
			}
			if decision.Version != tc.req.Version+1 {
				t.Errorf("version = %d, want %d", decision.Version, tc.req.Version+1)
			}
			switch {
			case tc.wantResume == nil && decision.ResumeState != nil:
				t.Errorf("resume_state = %s, want nil", *decision.ResumeState)
			case tc.wantResume != nil && decision.ResumeState == nil:
				t.Errorf("resume_state = nil, want %s", *tc.wantResume)
			case tc.wantResume != nil && *decision.ResumeState != *tc.wantResume:
				t.Errorf("resume_state = %s, want %s", *decision.ResumeState, *tc.wantResume)
			}
		})
	}
}

// TestNonOperationalHopsAreClosed is the gate-G4 negative test: a valid
// contract transition with a legitimate actor still cannot run in Phase 1.
func TestNonOperationalHopsAreClosed(t *testing.T) {
	held := Request{
		LeaseOwner: new("orch-1"), LeaseEpoch: 1, ExpectedLeaseEpoch: 1,
		LeaseExpiresAt: new(now.Add(time.Minute)), ActorType: ActorTypeSystem,
	}

	closed := []Request{
		withState(held, StateCoding, ActionAdvance, ActorOrchestrator),
		withState(held, StateVerifying, ActionAdvance, ActorOrchestrator),
		withState(held, StateQA, ActionAdvance, ActorOrchestrator),
	}
	for _, req := range closed {
		_, err := Evaluate(req, now)
		if !errors.Is(err, ErrStageNotOperational) {
			t.Errorf("%s --%s-->: got %v, want ErrStageNotOperational", req.State, req.Action, err)
		}
		if status := HTTPStatusForError(err); status != 409 {
			t.Errorf("%s --%s-->: status = %d, want 409", req.State, req.Action, status)
		}
	}
}

// TestForbiddenTransitions covers self-transitions, transitions out of
// terminal states and undefined pairs.
func TestForbiddenTransitions(t *testing.T) {
	held := Request{
		LeaseOwner: new("orch-1"), LeaseEpoch: 1, ExpectedLeaseEpoch: 1,
		LeaseExpiresAt: new(now.Add(time.Minute)),
	}

	tests := []struct {
		name string
		req  Request
		want error
	}{
		{"self transition", withState(held, StateCoding, ActionAdvance, ActorOrchestrator), ErrStageNotOperational},
		{"advance from queue", withState(Request{}, StateQueued, ActionAdvance, ActorOrchestrator), ErrInvalidTransition},
		{"advance from blocked", withState(Request{}, StateBlocked, ActionAdvance, ActorOrchestrator), ErrInvalidTransition},
		{"advance from retry_wait", withState(Request{}, StateRetryWait, ActionAdvance, ActorOrchestrator), ErrInvalidTransition},
		{"begin not from queued", withState(held, StateBlocked, ActionBegin, ActorScheduler), ErrInvalidTransition},
		{"unknown action", withState(Request{}, StateQueued, Action("teleport"), ActorScheduler), ErrInvalidTransition},
		{"cancel from completed", withState(Request{}, StateCompleted, ActionCancel, ActorOperator), ErrAlreadyTerminal},
		{"fail from failed", withState(Request{}, StateFailed, ActionFail, ActorPolicy), ErrAlreadyTerminal},
		{"resume needs resume_state", withState(Request{}, StateBlocked, ActionResume, ActorOperator), ErrInvalidTransition},
		{"wrong actor for begin", withState(held, StateQueued, ActionBegin, ActorOperator), ErrForbidden},
		{"wrong actor for cancel", withState(Request{}, StateQueued, ActionCancel, ActorScheduler), ErrForbidden},
		{"wrong actor for schedule_retry", withState(Request{}, StateQueued, ActionScheduleRetry, ActorOperator), ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Evaluate(tc.req, now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestErrorOrderStability pins the classifier order documented on Evaluate:
// stale_version → already_terminal → invalid_transition → stale_lease →
// forbidden → stage_not_operational.
func TestErrorOrderStability(t *testing.T) {
	owner, epoch, expired := new("orch-1"), int64(1), new(now.Add(-time.Minute))

	// stale_version wins over everything else, including a terminal state, a
	// foreign lease and an unparseable action.
	_, err := Evaluate(Request{
		State: StateCompleted, Version: 4, ExpectedVersion: 3,
		LeaseOwner: owner, LeaseEpoch: epoch, ExpectedLeaseEpoch: 99,
		LeaseExpiresAt: expired, Action: Action("teleport"), Actor: ActorPolicy,
	}, now)
	if !errors.Is(err, ErrStaleVersion) {
		t.Errorf("got %v, want ErrStaleVersion", err)
	}

	// already_terminal beats the lease rule: a terminal run is never resumable.
	_, err = Evaluate(Request{
		State: StateCompleted, Version: 3, ExpectedVersion: 3,
		LeaseOwner: owner, LeaseEpoch: epoch, ExpectedLeaseEpoch: 99,
		LeaseExpiresAt: expired, Action: ActionCancel, Actor: ActorOperator,
	}, now)
	if !errors.Is(err, ErrAlreadyTerminal) {
		t.Errorf("got %v, want ErrAlreadyTerminal", err)
	}

	// invalid_transition beats the lease rule and the actor rule, so a nonsense
	// action reports the same class no matter who calls it or what lease exists.
	_, err = Evaluate(Request{
		State: StateCoding, Version: 3, ExpectedVersion: 3,
		LeaseOwner: owner, LeaseEpoch: epoch, ExpectedLeaseEpoch: 1,
		LeaseExpiresAt: expired, Action: Action("teleport"), Actor: ActorPolicy,
	}, now)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("got %v, want ErrInvalidTransition", err)
	}

	// stale_lease beats the actor rule: the run moved on, and that outranks the
	// caller's lack of rights.
	_, err = Evaluate(Request{
		State: StateCoding, Version: 3, ExpectedVersion: 3,
		LeaseOwner: owner, LeaseEpoch: epoch, ExpectedLeaseEpoch: 99,
		LeaseExpiresAt: expired, Action: ActionBlock, Actor: ActorOrchestrator,
	}, now)
	if !errors.Is(err, ErrStaleLease) {
		t.Errorf("got %v, want ErrStaleLease", err)
	}
}

// TestLeaseBranches: the spec's two-branch epoch rule, with no epoch=0 bypass.
// TestRetryDueRequiresTheScheduledTime: the due-time guard is a transition
// rule, not a lease concern; a retry that is not due is an invalid transition.
func TestRetryDueRequiresTheScheduledTime(t *testing.T) {
	resume := new(StateCoding)
	tests := []struct {
		name  string
		dueAt *time.Time
		want  error
	}{
		{"no due time recorded", nil, ErrInvalidTransition},
		{"due in the future", new(now.Add(time.Minute)), ErrInvalidTransition},
		{"due exactly now", new(now), nil},
		{"overdue", new(now.Add(-time.Hour)), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Evaluate(Request{
				State: StateRetryWait, Action: ActionRetryDue, Actor: ActorScheduler,
				LeaseOwner: new("sched"), LeaseEpoch: 1, ExpectedLeaseEpoch: 1,
				LeaseExpiresAt: new(now.Add(time.Minute)),
				ResumeState:    resume, RetryDueAt: tc.dueAt,
			}, now)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("got %v, want success", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLeaseBranches(t *testing.T) {
	owner, live := new("orch-1"), new(now.Add(time.Minute))
	expired := new(now.Add(-time.Minute))

	tests := []struct {
		name string
		req  Request
		want error
	}{
		{
			"operator action with no lease is allowed by policy",
			withState(Request{}, StateQueued, ActionCancel, ActorOperator),
			nil,
		},
		{
			"blocked run can be cancelled without a lease",
			withState(Request{}, StateBlocked, ActionCancel, ActorOperator),
			nil,
		},
		{
			"begin without a lease is rejected",
			withState(Request{}, StateQueued, ActionBegin, ActorScheduler),
			ErrStaleLease,
		},
		{
			"begin with an expired lease is rejected",
			withState(Request{LeaseOwner: owner, LeaseEpoch: 2, ExpectedLeaseEpoch: 2, LeaseExpiresAt: expired}, StateQueued, ActionBegin, ActorScheduler),
			ErrStaleLease,
		},
		{
			"begin with a stale epoch is rejected",
			withState(Request{LeaseOwner: owner, LeaseEpoch: 2, ExpectedLeaseEpoch: 1, LeaseExpiresAt: live}, StateQueued, ActionBegin, ActorScheduler),
			ErrStaleLease,
		},
		{
			"begin with a live matching lease is accepted",
			withState(Request{LeaseOwner: owner, LeaseEpoch: 2, ExpectedLeaseEpoch: 2, LeaseExpiresAt: live}, StateQueued, ActionBegin, ActorScheduler),
			nil,
		},
		{
			"expected epoch 0 does not bypass the check",
			withState(Request{LeaseOwner: owner, LeaseEpoch: 2, ExpectedLeaseEpoch: 0, LeaseExpiresAt: live}, StateQueued, ActionBegin, ActorScheduler),
			ErrStaleLease,
		},
		{
			"expired lease blocks a policy action that would otherwise pass",
			withState(Request{LeaseOwner: owner, LeaseEpoch: 2, ExpectedLeaseEpoch: 2, LeaseExpiresAt: expired}, StateCoding, ActionBlock, ActorOperator),
			ErrStaleLease,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Evaluate(tc.req, now)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("got %v, want success", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	cases := map[error]int{
		ErrNotFound:            404,
		ErrStaleVersion:        409,
		ErrStaleLease:          409,
		ErrAlreadyTerminal:     409,
		ErrInvalidTransition:   409,
		ErrConflictActiveRun:   409,
		ErrStageNotOperational: 409,
		ErrForbidden:           403,
	}
	for err, want := range cases {
		if got := HTTPStatusForError(err); got != want {
			t.Errorf("%v: status = %d, want %d", err, got, want)
		}
	}
	if got := HTTPStatusForError(errors.New("boom")); got != 500 {
		t.Errorf("unknown error: status = %d, want 500", got)
	}
}

func withState(req Request, state State, action Action, actor Actor) Request {
	req.State = state
	req.Action = action
	req.Actor = actor
	if req.Version == 0 {
		req.Version = 1
	}
	if req.ExpectedVersion == 0 {
		req.ExpectedVersion = req.Version
	}
	if req.ActorType == "" {
		req.ActorType = ActorTypeSystem
	}
	return req
}
