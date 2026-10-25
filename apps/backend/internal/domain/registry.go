package domain

import (
	"errors"
	"fmt"
	"time"
)

// VTX-3 / SPEC-AUTO-02 registry state machine.
//
// This file is pure data + rules: no DB, no HTTP, no Temporal. The persistence
// layer locks the row and hands the loaded values to Evaluate; the decision it
// returns is what the application service writes inside the same transaction.

// State is a task-run state. The string values are the wire contract (spec
// §0.C) — every value appears literally in the DB CHECK constraint too.
type State string

const (
	StateQueued      State = "queued"
	StatePreparing   State = "preparing"
	StateCoding      State = "coding"
	StateVerifying   State = "verifying"
	StateQA          State = "qa"
	StateQualityGate State = "quality_gate"
	StatePRDraft     State = "pr_draft"
	StateCompleted   State = "completed"
	StateBlocked     State = "blocked"
	StateRetryWait   State = "retry_wait"
	StateFailed      State = "failed"
	StateCancelled   State = "cancelled"
)

// Action is a requested transition verb.
type Action string

const (
	ActionBegin         Action = "begin"
	ActionAdvance       Action = "advance"
	ActionComplete      Action = "complete"
	ActionBlock         Action = "block"
	ActionResume        Action = "resume"
	ActionScheduleRetry Action = "schedule_retry"
	ActionRetryDue      Action = "retry_due"
	ActionCancel        Action = "cancel"
	ActionFail          Action = "fail"
)

// Actor is the authenticated principal allowed to drive a transition.
type Actor string

const (
	ActorScheduler    Actor = "scheduler"
	ActorOrchestrator Actor = "orchestrator"
	ActorOperator     Actor = "operator"
	ActorPolicy       Actor = "policy"
)

// ActorType separates a machine caller from a human one in the audit trail.
type ActorType string

const (
	ActorTypeSystem ActorType = "system"
	ActorTypeHuman  ActorType = "human"
)

// Validation errors. Their HTTP statuses are fixed by spec §0.D; the transport
// layer maps them by identity, so the codes must stay stable.
var (
	ErrNotFound          = errors.New("registry: run not found")
	ErrStaleVersion      = errors.New("registry: stale version")
	ErrStaleLease        = errors.New("registry: stale lease")
	ErrAlreadyTerminal   = errors.New("registry: run already terminal")
	ErrInvalidTransition = errors.New("registry: invalid transition")
	ErrForbidden         = errors.New("registry: actor not permitted")
	ErrConflictActiveRun = errors.New("registry: task already has an active run")
	// ErrStageNotOperational is a VTX-3 addition (PO #4, gate G4): the hop is a
	// valid contract transition but its downstream system (VTX-4/5/7/13) does
	// not exist yet, so no runtime path may perform it. Reporting
	// invalid_transition here would be a lie about the contract; reporting
	// success would forge evidence. 409, like the other conflicts.
	ErrStageNotOperational = errors.New("registry: stage not operational in phase 1")
)

// Errors maps each typed error to its HTTP status (spec §0.D).
func HTTPStatusForError(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return 404
	case errors.Is(err, ErrStaleVersion), errors.Is(err, ErrStaleLease),
		errors.Is(err, ErrAlreadyTerminal), errors.Is(err, ErrInvalidTransition),
		errors.Is(err, ErrConflictActiveRun), errors.Is(err, ErrStageNotOperational),
		errors.Is(err, ErrEvidenceInvalid), errors.Is(err, ErrQAVerdictConflict),
		errors.Is(err, ErrQASessionNotUnique), errors.Is(err, ErrQAEvidenceMissing),
		errors.Is(err, ErrQASHAMismatch), errors.Is(err, ErrQARunNotInQA):
		return 409
	case errors.Is(err, ErrForbidden):
		return 403
	default:
		return 500
	}
}

// Valid reports whether the verb is one of the nine contract actions.
func (a Action) Valid() bool {
	switch a {
	case ActionBegin, ActionAdvance, ActionComplete, ActionBlock, ActionResume,
		ActionScheduleRetry, ActionRetryDue, ActionCancel, ActionFail:
		return true
	default:
		return false
	}
}

// Valid reports whether the value is one of the four authenticated actors.
func (a Actor) Valid() bool {
	switch a {
	case ActorScheduler, ActorOrchestrator, ActorOperator, ActorPolicy:
		return true
	default:
		return false
	}
}

// Terminal states (I-06). blocked/retry_wait stay nonterminal on purpose.
func (s State) IsTerminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

func (s State) IsNonterminal() bool { return !s.IsTerminal() }

// Valid reports whether the value is one of the twelve contract states.
func (s State) Valid() bool {
	switch s {
	case StateQueued, StatePreparing, StateCoding, StateVerifying, StateQA,
		StateQualityGate, StatePRDraft, StateCompleted, StateBlocked,
		StateRetryWait, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

// transition is one row of the §0.C table.
type transition struct {
	key         transitionKey
	to          State
	actors      []Actor
	operational bool // false ⇒ ErrStageNotOperational (gate G4)
}

type transitionKey struct {
	from   State
	action Action
}

// stageAdvance is the linear pipeline chain. Every hop is operational now
// that VTX-5..12 are merged; evidence enforcement lives in the transition
// guards (coding/verifying/qa/quality_gate) and the preparing readiness gate.
var stageAdvance = []State{
	StatePreparing, StateCoding, StateVerifying, StateQA, StateQualityGate, StatePRDraft,
}

func transitionTable() map[transitionKey]transition {
	rows := []transition{
		{key: transitionKey{StateQueued, ActionBegin}, to: StatePreparing, actors: []Actor{ActorScheduler}, operational: true},
		{key: transitionKey{StatePRDraft, ActionComplete}, to: StateCompleted, actors: []Actor{ActorOrchestrator}, operational: true},
	}
	for i, from := range stageAdvance {
		if i+1 >= len(stageAdvance) {
			break
		}
		rows = append(rows, transition{
			key:         transitionKey{from, ActionAdvance},
			to:          stageAdvance[i+1],
			actors:      []Actor{ActorOrchestrator},
			operational: true,
		})
	}

	// Side-effect transitions stay operational: they only move bookkeeping.
	for _, from := range append([]State{StateQueued}, stageAdvance...) {
		rows = append(rows,
			transition{key: transitionKey{from, ActionBlock}, to: StateBlocked, actors: []Actor{ActorOperator, ActorPolicy}, operational: true},
			transition{key: transitionKey{from, ActionScheduleRetry}, to: StateRetryWait, actors: []Actor{ActorPolicy}, operational: true},
		)
	}
	rows = append(rows,
		transition{key: transitionKey{StateBlocked, ActionResume}, to: "", actors: []Actor{ActorOperator}, operational: true},
		transition{key: transitionKey{StateRetryWait, ActionRetryDue}, to: "", actors: []Actor{ActorScheduler, ActorPolicy}, operational: true},
	)
	for _, from := range append([]State{StateQueued, StateBlocked, StateRetryWait}, stageAdvance...) {
		rows = append(rows,
			transition{key: transitionKey{from, ActionCancel}, to: StateCancelled, actors: []Actor{ActorOperator, ActorPolicy}, operational: true},
			transition{key: transitionKey{from, ActionFail}, to: StateFailed, actors: []Actor{ActorPolicy}, operational: true},
		)
	}

	table := make(map[transitionKey]transition, len(rows))
	for _, row := range rows {
		table[row.key] = row
	}
	return table
}

var transitionByKey = transitionTable()

// Request is the loaded run state plus the caller's expectation.
type Request struct {
	PreparationReady   bool
	State              State
	Version            int64
	ExpectedVersion    int64
	LeaseOwner         *string
	LeaseEpoch         int64
	ExpectedLeaseEpoch int64
	LeaseExpiresAt     *time.Time
	ResumeState        *State
	RetryDueAt         *time.Time
	Action             Action
	Actor              Actor
	ActorType          ActorType
}

// Decision is what the caller must persist. ResumeState is set only for the
// transitions that park the run (block, schedule_retry).
type Decision struct {
	From        State
	To          State
	ResumeState *State
	Action      Action
	Actor       Actor
	ActorType   ActorType
	// Version is the run version this transition produces (expected + 1).
	Version int64
}

// requiresLease: only a lease holder may perform these — they claim that real
// work is happening.
func (a Action) requiresLease() bool {
	switch a {
	case ActionBegin, ActionAdvance, ActionComplete:
		return true
	default:
		return false
	}
}

// isPolicyAction: operator/policy bookkeeping actions. The spec's epoch rule
// explicitly permits these without a lease while the run is queued/blocked.
func (a Action) isPolicyAction() bool {
	switch a {
	case ActionBlock, ActionResume, ActionScheduleRetry, ActionRetryDue, ActionCancel, ActionFail:
		return true
	default:
		return false
	}
}

// Evaluate applies the state machine. It never approves a non-operational
// hop: use EvaluateWithGuard only from inside an EvidenceGuard-backed
// transition (VTX-10), where the guard — not the table flag — is the
// operationality+evidence proof.
func Evaluate(req Request, now time.Time) (Decision, error) {
	return evaluate(req, now, false)
}

// EvaluateWithGuard is identical to Evaluate except a contract-valid hop
// flagged non-operational is approved: the caller's EvidenceGuard has already
// verified the required evidence inside the transition tx, which is the
// VTX-10 definition of "operational". A caller without a registered guard
// must use Evaluate — the table stays the closed default.
func EvaluateWithGuard(req Request, now time.Time) (Decision, error) {
	return evaluate(req, now, true)
}

// evaluate applies the state machine. Error classes are checked in this order:
//
//  1. stale_version    — someone else advanced the run
//  2. already_terminal — terminal runs never move (I-06)
//  3. invalid_transition — the (state, action) pair is not in the §0.C table
//  4. stale_lease      — lease/epoch rule (only defined for pairs that exist)
//  5. forbidden        — authenticated actor has no right to this pair
//  6. stage_not_operational — valid contract hop whose downstream system is absent
//
// Deviation from the literal §0.D listing (which prints stale_lease before
// already_terminal/invalid_transition): the two-branch lease rule is written in
// terms of "the action being performed", which only makes sense for a pair the
// table allows. Checking the pair first keeps error classes stable — a nonsense
// action reports invalid_transition whether or not a lease happens to be held,
// which is what negative tests and API consumers need. All five §0.D classes
// stay distinguishable, which is the property the order exists to protect.
// (not_found is decided by the repository before this is called.)
func evaluate(req Request, now time.Time, allowGuarded bool) (Decision, error) {
	if req.Version != req.ExpectedVersion {
		return Decision{}, fmt.Errorf("%w: have %d, expected %d", ErrStaleVersion, req.Version, req.ExpectedVersion)
	}

	if req.State.IsTerminal() {
		return Decision{}, fmt.Errorf("%w: state %s", ErrAlreadyTerminal, req.State)
	}

	row, ok := transitionByKey[transitionKey{req.State, req.Action}]
	if !ok {
		return Decision{}, fmt.Errorf("%w: %s --%s-->", ErrInvalidTransition, req.State, req.Action)
	}

	if err := checkLease(req, now); err != nil {
		return Decision{}, err
	}

	if !actorPermitted(row.actors, req.Actor) {
		return Decision{}, fmt.Errorf("%w: actor %s may not %s from %s", ErrForbidden, req.Actor, req.Action, req.State)
	}

	if !row.operational && !allowGuarded {
		return Decision{}, fmt.Errorf("%w: %s --%s-->", ErrStageNotOperational, req.State, req.Action)
	}

	if req.State == StatePreparing && req.Action == ActionAdvance && !req.PreparationReady {
		return Decision{}, ErrEvidenceInvalid
	}

	to := row.to
	var resume *State
	switch req.Action {
	case ActionRetryDue:
		// A retry is only due once its scheduled time has arrived; the
		// remaining budget belongs to VTX-5, not to this state machine.
		if req.RetryDueAt == nil || req.RetryDueAt.After(now) {
			return Decision{}, fmt.Errorf("%w: %s --retry_due--> before retry_due_at", ErrInvalidTransition, req.State)
		}
		fallthrough
	case ActionResume:
		// Resume targets the state the run parked from; without it the run
		// would have nowhere to go, so a missing resume_state is a corrupt row.
		if req.ResumeState == nil || !req.ResumeState.Valid() {
			return Decision{}, fmt.Errorf("%w: %s --%s--> without resume_state", ErrInvalidTransition, req.State, req.Action)
		}
		to = *req.ResumeState
	case ActionBlock, ActionScheduleRetry:
		parked := req.State
		resume = &parked
	}

	return Decision{
		From:        req.State,
		To:          to,
		ResumeState: resume,
		Action:      req.Action,
		Actor:       req.Actor,
		ActorType:   req.ActorType,
		Version:     req.Version + 1,
	}, nil
}

func actorPermitted(allowed []Actor, actor Actor) bool {
	for _, a := range allowed {
		if a == actor {
			return true
		}
	}
	return false
}

// checkLease implements spec §0.D's two explicit branches — there is no
// "expectedEpoch == 0 ⇒ skip" bypass.
func checkLease(req Request, now time.Time) error {
	hasLease := req.LeaseOwner != nil && *req.LeaseOwner != ""
	leaseValid := hasLease &&
		req.LeaseEpoch == req.ExpectedLeaseEpoch &&
		req.LeaseExpiresAt != nil && req.LeaseExpiresAt.After(now)

	switch {
	case req.Action.requiresLease():
		if !leaseValid {
			return fmt.Errorf("%w: action %s requires an unexpired lease", ErrStaleLease, req.Action)
		}
	case req.Action.isPolicyAction():
		// §0.D: epoch/ownership/expiry are compared "(if an active lease
		// exists)". No active lease -> the policy action is allowed from any
		// nonterminal state; queued/blocked are merely the usual pre-lease
		// states. With a lease present, a foreign/expired one is a hard fail.
		if hasLease && !leaseValid {
			return fmt.Errorf("%w: action %s presented an expired/foreign lease", ErrStaleLease, req.Action)
		}
	default:
		// Unknown action: the lease rule says nothing, the transition map
		// decides (invalid_transition).
	}
	return nil
}
