package application_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
)

type fakeOutbox struct {
	messages   []application.OutboxMessage
	claimedFor []string
	claimed    []string
	acked      []string
	retried    []string
	retryAfter []time.Duration
	quarantine map[string]string
	claimErr   error
	trace      []string
	leak       bool
}

func newFakeOutbox(messages ...application.OutboxMessage) *fakeOutbox {
	return &fakeOutbox{messages: messages, quarantine: map[string]string{}}
}

func (f *fakeOutbox) ClaimOutbox(_ context.Context, consumer string, eventTypes []string, limit int32, _ time.Duration) ([]application.OutboxMessage, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	f.claimedFor = eventTypes
	out := make([]application.OutboxMessage, 0, len(f.messages))
	for _, message := range f.messages {
		matches := f.leak
		for _, eventType := range eventTypes {
			matches = matches || message.EventType == eventType
		}
		if matches && int32(len(out)) < limit {
			out = append(out, message)
			f.claimed = append(f.claimed, message.ID)
		}
	}
	return out, nil
}

func (f *fakeOutbox) MarkPublished(_ context.Context, ids []string) (int64, error) {
	f.acked = append(f.acked, ids...)
	f.trace = append(f.trace, "ack")
	return int64(len(ids)), nil
}

func (f *fakeOutbox) ScheduleRetry(_ context.Context, ids []string, backoff time.Duration) (int64, error) {
	f.retried = append(f.retried, ids...)
	f.retryAfter = append(f.retryAfter, backoff)
	return int64(len(ids)), nil
}

func (f *fakeOutbox) Quarantine(_ context.Context, id, reason string) (int64, error) {
	f.quarantine[id] = reason
	return 1, nil
}

func (f *fakeOutbox) OutboxLag(context.Context, string) (int64, *time.Time, error) {
	return 0, nil, nil
}

func newDispatcher(store application.OutboxStore, handlers map[string]application.EventHandler, cfg application.OutboxDispatcherConfig) *application.OutboxDispatcher {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return application.NewOutboxDispatcher(store, "registry-intake", handlers, logger, cfg)
}

func message(id, eventType string, attempt int32) application.OutboxMessage {
	return application.OutboxMessage{ID: id, IntentID: "intent-1", EventType: eventType, Payload: []byte(`{}`), Attempt: attempt}
}

// TestDispatchOnceAcksOnlyAfterTheEffect: TR-20 — the ACK must be written after
// the handler's own transaction committed, so a crash in between replays.
func TestDispatchOnceAcksOnlyAfterTheEffect(t *testing.T) {
	store := newFakeOutbox(message("evt-1", application.OutboxEventExecutionIntentCreated, 0))
	handler := func(context.Context, []byte) error {
		store.trace = append(store.trace, "effect")
		return nil
	}
	dispatcher := newDispatcher(store, map[string]application.EventHandler{
		application.OutboxEventExecutionIntentCreated: handler,
	}, application.OutboxDispatcherConfig{})

	stats, err := dispatcher.DispatchOnce(context.Background())
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if stats.Published != 1 || stats.Quarantined != 0 || stats.Retried != 0 {
		t.Errorf("stats = %+v, want 1 published", stats)
	}
	if len(store.trace) != 2 || store.trace[0] != "effect" || store.trace[1] != "ack" {
		t.Errorf("trace = %v, want [effect ack]", store.trace)
	}
}

// TestDispatchOnceClaimsOnlyItsOwnEventTypes: §0.E routing guard — the registry
// consumer asks for `execution_intent.created.v1` only, so its own
// `task_run.*` output stays pending for VTX-5 instead of feeding back in.
// TestRetryDelayGrowsExponentially: the runbook promises
// min(base·2^attempt, cap) + jitter, so attempt 1 must wait about twice as
// long as attempt 0 — a fixed 5s delay would exhaust MaxAttempts in ~25s.
func TestRetryDelayGrowsExponentially(t *testing.T) {
	base := 5 * time.Second
	handler := func(context.Context, []byte) error { return errors.New("transient") }
	for _, tc := range []struct {
		attempt int32
		min     time.Duration
		max     time.Duration
	}{
		{attempt: 0, min: base, max: base + base},
		{attempt: 1, min: 2 * base, max: 3 * base},
		{attempt: 2, min: 4 * base, max: 5 * base},
		{attempt: 40, min: 5 * time.Minute, max: 5*time.Minute + base},
	} {
		fake := newFakeOutbox(message("evt-1", "execution_intent.created.v1", tc.attempt))
		d := newDispatcher(fake, map[string]application.EventHandler{"execution_intent.created.v1": handler},
			application.OutboxDispatcherConfig{Backoff: base, MaxAttempts: 100})
		if _, err := d.DispatchOnce(context.Background()); err != nil {
			t.Fatalf("attempt %d: %v", tc.attempt, err)
		}
		if len(fake.retryAfter) != 1 {
			t.Fatalf("attempt %d: retryAfter = %v, want one schedule", tc.attempt, fake.retryAfter)
		}
		got := fake.retryAfter[0]
		if got < tc.min || got > tc.max {
			t.Errorf("attempt %d backoff = %v, want within [%v, %v]", tc.attempt, got, tc.min, tc.max)
		}
	}
}

func TestDispatchOnceClaimsOnlyItsOwnEventTypes(t *testing.T) {
	store := newFakeOutbox(
		message("evt-intent", application.OutboxEventExecutionIntentCreated, 0),
		message("evt-run-created", application.OutboxEventTaskRunCreated, 0),
		message("evt-run-state", application.OutboxEventTaskRunStateChanged, 0),
	)
	dispatcher := newDispatcher(store, map[string]application.EventHandler{
		application.OutboxEventExecutionIntentCreated: func(context.Context, []byte) error { return nil },
	}, application.OutboxDispatcherConfig{})

	stats, err := dispatcher.DispatchOnce(context.Background())
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if stats.Claimed != 1 || len(store.acked) != 1 || store.acked[0] != "evt-intent" {
		t.Errorf("acked %v, want only the intent event", store.acked)
	}
	if len(store.claimedFor) != 1 || store.claimedFor[0] != application.OutboxEventExecutionIntentCreated {
		t.Errorf("claim filter = %v, want only the intent event type", store.claimedFor)
	}
}

// TestDispatchOnceQuarantinesPoisonAndExhaustedRetries: TR-12/TR-13/TR-22 — an
// unhandled event type and a payload-invalid event are quarantined immediately;
// a transient failure retries with bounded backoff until attempts run out.
func TestDispatchOnceQuarantinesPoisonAndExhaustedRetries(t *testing.T) {
	cases := []struct {
		name        string
		messages    []application.OutboxMessage
		handlerErr  error
		cfg         application.OutboxDispatcherConfig
		wantRetried int
		wantReason  string
		leak        bool
	}{
		{
			// A store that hands back an unhandled type must be quarantined,
			// not nil-panicked: the claim set is the routing guard, this is the
			// backstop.
			name:       "unhandled event type",
			messages:   []application.OutboxMessage{message("evt-unknown", "task_run.mystery.v1", 0)},
			wantReason: "unsupported_event_type",
			leak:       true,
		},
		{
			name:       "poison payload",
			messages:   []application.OutboxMessage{message("evt-poison", application.OutboxEventExecutionIntentCreated, 0)},
			handlerErr: fmt.Errorf("%w: issue_key is required", application.ErrPayloadInvalid),
			wantReason: "issue_key is required",
		},
		{
			name:       "retries exhausted",
			messages:   []application.OutboxMessage{message("evt-tired", application.OutboxEventExecutionIntentCreated, 4)},
			handlerErr: errors.New("database is on fire"),
			cfg:        application.OutboxDispatcherConfig{MaxAttempts: 5},
		},
		{
			name:        "transient failure retries",
			messages:    []application.OutboxMessage{message("evt-transient", application.OutboxEventExecutionIntentCreated, 0)},
			handlerErr:  errors.New("connection reset"),
			cfg:         application.OutboxDispatcherConfig{Backoff: time.Second},
			wantRetried: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeOutbox(tc.messages...)
			store.leak = tc.leak
			dispatcher := newDispatcher(store, map[string]application.EventHandler{
				application.OutboxEventExecutionIntentCreated: func(context.Context, []byte) error { return tc.handlerErr },
			}, tc.cfg)

			stats, err := dispatcher.DispatchOnce(context.Background())
			if err != nil {
				t.Fatalf("DispatchOnce: %v", err)
			}
			if stats.Published != 0 {
				t.Errorf("published = %d, want 0", stats.Published)
			}
			if len(store.retried) != tc.wantRetried {
				t.Errorf("retried = %v, want %d", store.retried, tc.wantRetried)
			}
			if tc.wantRetried > 0 {
				if len(store.retryAfter) != 1 || store.retryAfter[0] <= 0 || store.retryAfter[0] > 2*tc.cfg.Backoff {
					t.Errorf("backoff = %v, want one value within (0, 2*%v]", store.retryAfter, tc.cfg.Backoff)
				}
				if len(store.quarantine) != 0 {
					t.Errorf("transient failure was quarantined: %v", store.quarantine)
				}
				return
			}
			if len(store.quarantine) != 1 {
				t.Fatalf("quarantine = %v, want exactly one entry", store.quarantine)
			}
			for _, reason := range store.quarantine {
				if tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
					t.Errorf("quarantine reason = %q, want it to contain %q", reason, tc.wantReason)
				}
			}
		})
	}
}

// TestRunStopsOnContextCancel: the dispatcher is a long-running process; a
// cancelled context (SIGTERM) must end the loop without an error.
func TestRunStopsOnContextCancel(t *testing.T) {
	store := newFakeOutbox()
	dispatcher := newDispatcher(store, map[string]application.EventHandler{
		application.OutboxEventExecutionIntentCreated: func(context.Context, []byte) error { return nil },
	}, application.OutboxDispatcherConfig{PollInterval: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancel")
	}
}
