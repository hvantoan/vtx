package retry

import (
	"context"
	"errors"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"math"
	"testing"
	"time"
)

func TestRetryBackoffBounds(t *testing.T) {
	p := DefaultPolicy()
	p.Random = func() float64 { return .5 }
	if p.Backoff(1) != 30*time.Second || p.Backoff(2) != time.Minute || p.Backoff(math.MaxInt32) != 10*time.Minute {
		t.Fatal("incorrect exponential cap")
	}
	for _, sample := range []float64{0, .25, .5, .75, 1} {
		p.Random = func() float64 { return sample }
		for n := int32(1); n < 30; n++ {
			d := p.Backoff(n)
			base := p
			base.JitterFrac = 0
			want := base.Backoff(n)
			if float64(d) < .8*float64(want) || float64(d) > 1.2*float64(want) {
				t.Fatalf("jitter outside bounds: %v", d)
			}
		}
	}
}
func TestRetryClassifier(t *testing.T) {
	for _, tc := range []struct {
		err   error
		class FailureClass
	}{{ErrAgentCrashed, ClassAgentCrashed}, {context.DeadlineExceeded, ClassAgentCrashed}, {domain.ErrStaleLease, ClassLeaseLost}, {application.ErrWorktreeNotFound, ClassWorktreeLost}, {application.ErrPayloadInvalid, ClassNonRetryable}, {application.ErrVerificationFailed, ClassVerificationFail}, {domain.ErrEvidenceInvalid, ClassEvidencePending}, {errors.New("network"), ClassInfraTransient}} {
		if got := Classify(tc.err); got != tc.class {
			t.Fatalf("%v: %s", tc.err, got)
		}
	}
}

type memoryRegistry struct {
	run   application.RunSnapshot
	plans []application.TransitionPlan
}

func (s *memoryRegistry) ConsumeIntent(context.Context, application.ConsumePlan, application.IntentEvent) (application.ConsumeOutcome, error) {
	return application.ConsumeOutcome{}, nil
}
func (s *memoryRegistry) TransitionRun(_ context.Context, _ application.TransitionCommand, decide func(application.RunSnapshot) (application.TransitionPlan, error)) (application.RunSnapshot, error) {
	p, err := decide(s.run)
	if err != nil {
		return s.run, err
	}
	s.plans = append(s.plans, p)
	s.run.State = p.Update.State
	s.run.Version = p.Audit.Version
	s.run.Attempt = p.Update.Attempt
	s.run.ResumeState = p.Update.ResumeState
	s.run.RetryDueAt = p.Update.RetryDueAt
	if p.Update.RetryPolicyVersion != nil {
		s.run.RetryPolicyVersion = *p.Update.RetryPolicyVersion
	}
	return s.run, nil
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }
func TestRetryBudgetExhaustionIsTerminalWithAudit(t *testing.T) {
	now := &clock{time.Now()}
	store := &memoryRegistry{run: application.RunSnapshot{RunID: "run", State: domain.StateCoding}}
	p := DefaultPolicy()
	p.JitterFrac = 0
	registry := application.NewRegistryService(store, "", now, nil).WithRetryPlanner(p)
	service := Service{Registry: registry}
	for n := int32(0); n < 3; n++ {
		run, err := service.HandleFailure(context.Background(), store.run, ClassInfraTransient, errors.New("transient"))
		if err != nil {
			t.Fatal(err)
		}
		if run.State != domain.StateRetryWait || run.Attempt != n+1 {
			t.Fatalf("%+v", run)
		}
		now.now = *run.RetryDueAt
		_, err = registry.TransitionRun(context.Background(), application.TransitionCommand{RunID: "run", ExpectedVersion: run.Version, Action: domain.ActionRetryDue, Actor: domain.ActorPolicy, ActorType: domain.ActorTypeSystem})
		if err != nil {
			t.Fatal(err)
		}
	}
	run, err := service.HandleFailure(context.Background(), store.run, ClassInfraTransient, errors.New("again"))
	if err != nil {
		t.Fatal(err)
	}
	last := store.plans[len(store.plans)-1]
	if !run.State.IsTerminal() || run.State != domain.StateFailed || *last.Update.ErrorCode != "retry_budget_exhausted" || string(last.Audit.Metadata) == "" || *last.Update.RetryPolicyVersion != "v1" {
		t.Fatalf("run=%+v plan=%+v", run, last)
	}
	if _, err := service.HandleFailure(context.Background(), store.run, ClassInfraTransient, nil); !errors.Is(err, domain.ErrAlreadyTerminal) {
		t.Fatalf("terminal revived: %v", err)
	}
}
func TestRetryFunctionalFailuresAndPoison(t *testing.T) {
	p := DefaultPolicy()
	for _, tc := range []struct {
		class  FailureClass
		action domain.Action
	}{{ClassVerificationFail, domain.ActionFail}, {ClassQAVerdictFail, domain.ActionFail}, {ClassGateFail, domain.ActionFail}, {ClassNonRetryable, domain.ActionBlock}, {ClassOperatorBlocked, domain.ActionBlock}, {"unknown", domain.ActionBlock}} {
		c, err := p.PlanFailure(application.RunSnapshot{State: domain.StateQA}, application.TransitionCommand{FailureClass: string(tc.class)}, time.Now())
		if err != nil || c.Action != tc.action || c.RetryDueAt != nil {
			t.Fatalf("%s %+v %v", tc.class, c, err)
		}
	}
}
func TestRecoveryCommandRejectsNonSystemActor(t *testing.T) {
	s := application.NewRegistryService(&memoryRegistry{}, "", &clock{}, nil).WithRetryPlanner(DefaultPolicy())
	for _, c := range []application.TransitionCommand{{RunID: "run", Action: domain.ActionScheduleRetry, Actor: domain.ActorOperator, ActorType: domain.ActorTypeHuman, RecoverExpiredLease: true}, {RunID: "run", Action: domain.ActionScheduleRetry, Actor: domain.ActorPolicy, ActorType: domain.ActorTypeHuman, RecoverExpiredLease: true}} {
		if _, err := s.TransitionRun(context.Background(), c); !errors.Is(err, domain.ErrForbidden) {
			t.Fatal(err)
		}
	}
}
