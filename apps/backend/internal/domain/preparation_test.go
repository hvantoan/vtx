package domain

import (
	"errors"
	"testing"
	"time"
)

func TestREQ06PreparationRequiresEvidence(t *testing.T) {
	r := Request{State: StatePreparing, Action: ActionAdvance, Actor: ActorOrchestrator, LeaseOwner: new("worker"), LeaseEpoch: 1, ExpectedLeaseEpoch: 1, LeaseExpiresAt: new(now.Add(time.Minute))}
	if _, err := Evaluate(r, now); !errors.Is(err, ErrEvidenceInvalid) || HTTPStatusForError(err) != 409 {
		t.Fatalf("missing evidence: %v", err)
	}
	r.PreparationReady = true
	d, err := Evaluate(r, now)
	if err != nil || d.To != StateCoding {
		t.Fatalf("ready evidence: %+v %v", d, err)
	}
}
