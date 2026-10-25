package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
)

var testNow = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)

type fixedClock struct{}

func (fixedClock) Now() time.Time { return testNow }

type memoryStore struct {
	runs       []application.RunSnapshot
	deferred   []string
	promoted   []string
	events     []string
	wrongEpoch bool
}

func (m *memoryStore) Transaction(ctx context.Context, fn func(Tx) error) error {
	copyStore := *m
	copyStore.runs = append([]application.RunSnapshot{}, m.runs...)
	copyStore.events = append([]string{}, m.events...)
	copyStore.deferred = append([]string{}, m.deferred...)
	copyStore.promoted = append([]string{}, m.promoted...)
	if err := fn(&copyStore); err != nil {
		return err
	}
	*m = copyStore
	return nil
}
func (m *memoryStore) Heartbeat(context.Context, string, int) error { return nil }
func (m *memoryStore) LiveSlots(context.Context) (int, error) {
	n := 0
	for _, r := range m.runs {
		if !r.State.IsTerminal() && (r.State == domain.StatePreparing || r.State == domain.StateCoding || r.State == domain.StateBlocked || r.LeaseExpiresAt != nil && r.LeaseExpiresAt.After(testNow)) {
			n++
		}
	}
	return n, nil
}
func (m *memoryStore) Queued(_ context.Context, limit int) ([]application.RunSnapshot, error) {
	out := []application.RunSnapshot{}
	for _, r := range m.runs {
		if r.State == domain.StateQueued && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}
func (m *memoryStore) Grant(_ context.Context, id, owner string, expiry time.Time) (int64, error) {
	for i, r := range m.runs {
		if r.RunID == id {
			r.LeaseEpoch++
			r.LeaseOwner = &owner
			r.LeaseExpiresAt = &expiry
			m.runs[i] = r
			return r.LeaseEpoch, nil
		}
	}
	return 0, domain.ErrNotFound
}
func (m *memoryStore) Transition(_ context.Context, cmd application.TransitionCommand) (application.RunSnapshot, error) {
	for i, r := range m.runs {
		if r.RunID == cmd.RunID {
			d, err := domain.Evaluate(domain.Request{State: r.State, Version: r.Version, ExpectedVersion: cmd.ExpectedVersion, LeaseOwner: r.LeaseOwner, LeaseEpoch: r.LeaseEpoch, ExpectedLeaseEpoch: cmd.ExpectedLeaseEpoch, LeaseExpiresAt: r.LeaseExpiresAt, Action: cmd.Action, Actor: cmd.Actor, ActorType: cmd.ActorType}, testNow)
			if err != nil {
				return r, err
			}
			r.State = d.To
			r.Version = d.Version
			m.runs[i] = r
			return r, nil
		}
	}
	return application.RunSnapshot{}, domain.ErrNotFound
}
func (m *memoryStore) Emit(_ context.Context, _ application.RunSnapshot, kind string, _ map[string]any) error {
	m.events = append(m.events, kind)
	return nil
}
func (m *memoryStore) Promote(context.Context, time.Time) error {
	if len(m.deferred) == 0 {
		return nil
	}
	for _, r := range m.runs {
		if r.TaskID == "task" && !r.State.IsTerminal() {
			return nil
		}
	}
	id := m.deferred[0]
	m.deferred = m.deferred[1:]
	m.promoted = append(m.promoted, id)
	m.runs = append(m.runs, application.RunSnapshot{RunID: id, TaskID: "task", State: domain.StateQueued})
	return nil
}
func (m *memoryStore) Expired(context.Context) ([]application.RunSnapshot, error) {
	out := []application.RunSnapshot{}
	for _, r := range m.runs {
		if !r.State.IsTerminal() && r.LeaseExpiresAt != nil && r.LeaseExpiresAt.Before(testNow) {
			out = append(out, r)
		}
	}
	return out, nil
}
func (m *memoryStore) Due(context.Context) ([]application.RunSnapshot, error) { return nil, nil }
func (m *memoryStore) Release(_ context.Context, r application.RunSnapshot) error {
	if m.wrongEpoch {
		return domain.ErrStaleLease
	}
	for i, v := range m.runs {
		if v.RunID == r.RunID {
			if v.LeaseEpoch != r.LeaseEpoch || v.LeaseOwner == nil || r.LeaseOwner == nil || *v.LeaseOwner != *r.LeaseOwner {
				return domain.ErrStaleLease
			}
			v.LeaseOwner = nil
			v.LeaseExpiresAt = nil
			m.runs[i] = v
			return nil
		}
	}
	return domain.ErrNotFound
}
func (m *memoryStore) SetError(context.Context, string, string) error { return nil }
func queued(n int) *memoryStore {
	m := &memoryStore{}
	for i := 0; i < n; i++ {
		m.runs = append(m.runs, application.RunSnapshot{RunID: fmt.Sprint(i), TaskID: fmt.Sprint(i), State: domain.StateQueued})
	}
	return m
}
func TestSCH01AdmissionCapDefersFourthCodingRun(t *testing.T) {
	m := queued(4)
	svc := New(m, Config{}, fixedClock{}, nil, nil)
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, _ := m.LiveSlots(context.Background())
	if live != 3 || m.runs[3].State != domain.StateQueued {
		t.Fatalf("live=%d overflow=%s", live, m.runs[3].State)
	}
	if len(m.events) != 3 {
		t.Fatalf("admission events=%d", len(m.events))
	}
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.runs[3].LeaseEpoch != 0 {
		t.Fatal("overflow received a lease")
	}
}
func TestSCH03BeginRequiresCurrentUnexpiredFencingToken(t *testing.T) {
	owner := "worker"
	expiry := testNow.Add(time.Minute)
	for _, tc := range []struct {
		name     string
		expected int64
		expires  *time.Time
		want     error
	}{{"missing", 1, nil, domain.ErrStaleLease}, {"stale", 1, &expiry, domain.ErrStaleLease}, {"expired", 2, new(testNow.Add(-time.Second)), domain.ErrStaleLease}, {"current", 2, &expiry, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := domain.Evaluate(domain.Request{State: domain.StateQueued, Action: domain.ActionBegin, Actor: domain.ActorScheduler, LeaseOwner: &owner, LeaseEpoch: 2, ExpectedLeaseEpoch: tc.expected, LeaseExpiresAt: tc.expires}, testNow)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}
func TestSCH06DeferredPromotionFIFOAfterSlotFrees(t *testing.T) {
	m := queued(1)
	m.runs[0].TaskID = "task"
	m.deferred = []string{"first", "second"}
	svc := New(m, Config{}, fixedClock{}, nil, nil)
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.promoted) != 0 {
		t.Fatal("promoted while task active")
	}
	m.runs[0].State = domain.StateCompleted
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.promoted) != 1 || m.promoted[0] != "first" {
		t.Fatalf("promoted %v", m.promoted)
	}
	m.runs[1].State = domain.StateCompleted
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.promoted) != 2 || m.promoted[1] != "second" {
		t.Fatalf("promoted %v", m.promoted)
	}
}
func TestSCH05RecoveryReleaseAndBlockAtomic(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(fmt.Sprint(wrong), func(t *testing.T) {
			m := queued(1)
			m.runs[0].State = domain.StateCoding
			m.runs[0].LeaseOwner = new("worker")
			m.runs[0].LeaseEpoch = 3
			m.runs[0].LeaseExpiresAt = new(testNow.Add(-time.Second))
			m.wrongEpoch = wrong
			err := New(m, Config{}, fixedClock{}, nil, nil).Tick(context.Background())
			if wrong {
				if !errors.Is(err, domain.ErrStaleLease) || m.runs[0].State != domain.StateCoding || m.runs[0].LeaseOwner == nil || len(m.events) != 0 {
					t.Fatalf("non-atomic failure: %v %+v", err, m)
				}
			} else {
				if err != nil || m.runs[0].State != domain.StateBlocked || m.runs[0].LeaseOwner != nil {
					t.Fatalf("recovery failed: %v %+v", err, m)
				}
				live, _ := m.LiveSlots(context.Background())
				if live != 1 {
					t.Fatal("unconfirmed stop freed slot")
				}
			}
		})
	}
}

type failingGuard struct{}

func (failingGuard) Eligible(context.Context, string) (bool, error) {
	return false, errors.New("unreachable")
}
func TestSCH10JiraGuardFailClosedAndDefaultOff(t *testing.T) {
	for _, on := range []bool{false, true} {
		m := queued(1)
		if err := New(m, Config{JiraGuard: on}, fixedClock{}, failingGuard{}, nil).Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if (m.runs[0].State == domain.StateQueued) != on {
			t.Fatalf("guard=%v state=%s", on, m.runs[0].State)
		}
	}
}
