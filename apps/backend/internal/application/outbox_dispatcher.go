package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// VTX-3 outbox dispatcher (§0.E): ONE designated consumer identity for
// `execution_intent.created.v1`. N replicas may run it — the claim query uses
// SKIP LOCKED, so they spread the work and never double-process an event.
//
// Delivery contract: at-least-once delivery, exactly-once persisted effects.
// The effect (ConsumeIntent) commits in its own transaction; the ACK
// (published_at) is written afterwards in a separate statement. A crash in
// between replays the event, and the inbox receipt makes the replay a no-op.

// OutboxMessage is one claimed outbox row.
type OutboxMessage struct {
	ID        string
	IntentID  string
	EventType string
	Payload   []byte
	Attempt   int32
	CreatedAt time.Time
}

// OutboxStore is the claim/ack surface. One pgx implementation.
type OutboxStore interface {
	// ClaimOutbox atomically stamps claimed_by/claimed_until on up to limit
	// claimable rows and returns them. Rows whose claim expired are reclaimable.
	ClaimOutbox(ctx context.Context, consumer string, eventTypes []string, limit int32, claimTTL time.Duration) ([]OutboxMessage, error)
	// MarkPublished is the consumer-specific ACK; it only touches rows that are
	// still unpublished, so a repeated ACK is harmless.
	MarkPublished(ctx context.Context, ids []string) (int64, error)
	ScheduleRetry(ctx context.Context, ids []string, backoff time.Duration) (int64, error)
	Quarantine(ctx context.Context, id, reason string) (int64, error)
	OutboxLag(ctx context.Context, eventType string) (int64, *time.Time, error)
}

// EventHandler applies one event. Returning ErrPayloadInvalid (or a wrapper)
// means "poison": the dispatcher quarantines instead of retrying.
type EventHandler func(ctx context.Context, payload []byte) error

// OutboxDispatcherConfig carries the operational knobs. Zero values fall back
// to the defaults, so callers configure only what they must.
type OutboxDispatcherConfig struct {
	BatchSize    int32
	ClaimTTL     time.Duration
	Backoff      time.Duration
	MaxBackoff   time.Duration
	PollInterval time.Duration
	MaxAttempts  int32
}

func (c OutboxDispatcherConfig) withDefaults() OutboxDispatcherConfig {
	if c.BatchSize <= 0 {
		c.BatchSize = 20
	}
	if c.ClaimTTL <= 0 {
		c.ClaimTTL = 30 * time.Second
	}
	if c.Backoff <= 0 {
		c.Backoff = 5 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 5 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	return c
}

// DispatchStats is what one pass did.
type DispatchStats struct {
	Claimed     int
	Published   int64
	Retried     int64
	Quarantined int
}

type OutboxDispatcher struct {
	store    OutboxStore
	consumer string
	handlers map[string]EventHandler
	// eventTypes is derived from handlers: the dispatcher can only ever claim
	// what it knows how to consume, which is also what keeps registry-published
	// `task_run.*` events out of the registry's own intake.
	eventTypes []string
	cfg        OutboxDispatcherConfig
	logger     *slog.Logger
}

func NewOutboxDispatcher(store OutboxStore, consumer string, handlers map[string]EventHandler, logger *slog.Logger, cfg OutboxDispatcherConfig) *OutboxDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	types := make([]string, 0, len(handlers))
	for eventType := range handlers {
		types = append(types, eventType)
	}
	return &OutboxDispatcher{
		store:      store,
		consumer:   consumer,
		handlers:   handlers,
		eventTypes: types,
		cfg:        cfg.withDefaults(),
		logger:     logger,
	}
}

// Run drains the outbox until ctx is cancelled. It never exits on a transient
// failure — that is what the retry/backoff columns are for.
func (d *OutboxDispatcher) Run(ctx context.Context) error {
	d.logger.Info("registry dispatcher started",
		"consumer", d.consumer, "event_types", d.eventTypes, "batch_size", d.cfg.BatchSize)
	for {
		stats, err := d.DispatchOnce(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			d.logger.Info("registry dispatcher stopped", "consumer", d.consumer)
			return nil
		case err != nil:
			// A persistent store failure (Postgres down, pool exhausted) must
			// not spin: back off on the same cadence as an empty claim.
			d.logger.Error("registry dispatcher pass failed", "consumer", d.consumer, "error", err)
			if !sleepCtx(ctx, d.cfg.PollInterval) {
				return nil
			}
		case stats.Claimed == 0:
			if !sleepCtx(ctx, d.cfg.PollInterval) {
				return nil
			}
		}
	}
}

// DispatchOnce claims and processes at most one batch.
func (d *OutboxDispatcher) DispatchOnce(ctx context.Context) (DispatchStats, error) {
	var stats DispatchStats
	messages, err := d.store.ClaimOutbox(ctx, d.consumer, d.eventTypes, d.cfg.BatchSize, d.cfg.ClaimTTL)
	if err != nil {
		return stats, fmt.Errorf("claim outbox: %w", err)
	}
	stats.Claimed = len(messages)
	if len(messages) == 0 {
		return stats, nil
	}

	var published []string
	// Retries are grouped by attempt: the delay is per-event
	// (min(base·2^attempt, cap) + jitter), so one batch can need several.
	retried := map[int32][]string{}
	for _, message := range messages {
		log := d.logger.With("outbox_id", message.ID, "event_type", message.EventType,
			"intent_id", message.IntentID, "attempt", message.Attempt, "consumer", d.consumer)

		handler, ok := d.handlers[message.EventType]
		if !ok {
			d.quarantine(ctx, message, "unsupported_event_type", &stats)
			continue
		}

		err := handler(ctx, message.Payload)
		switch {
		case err == nil:
			published = append(published, message.ID)
			log.Info("outbox event processed", "metric", "outbox_published")
		case errors.Is(err, ErrPayloadInvalid):
			log.Warn("outbox event quarantined", "metric", "outbox_dlq", "error_category", "payload_invalid", "error", err)
			d.quarantine(ctx, message, sanitizeReason(err.Error()), &stats)
		case message.Attempt+1 >= d.cfg.MaxAttempts:
			log.Error("outbox event retries exhausted", "metric", "outbox_dlq", "error_category", "retry_exhausted", "error", err)
			d.quarantine(ctx, message, fmt.Sprintf("retry_exhausted: %s", sanitizeReason(err.Error())), &stats)
		default:
			retried[message.Attempt] = append(retried[message.Attempt], message.ID)
			log.Warn("outbox event deferred", "metric", "outbox_retry", "error_category", "transient", "error", err)
		}
	}

	// ACK only after the effects committed (each handler owned its own tx).
	if len(published) > 0 {
		acked, err := d.store.MarkPublished(ctx, published)
		if err != nil {
			return stats, fmt.Errorf("ack %d events: %w", len(published), err)
		}
		stats.Published = acked
	}
	for attempt, ids := range retried {
		backoff := d.retryDelay(attempt)
		scheduled, err := d.store.ScheduleRetry(ctx, ids, backoff)
		if err != nil {
			return stats, fmt.Errorf("schedule retry: %w", err)
		}
		stats.Retried += scheduled
		d.logger.Info("outbox retry scheduled", "metric", "outbox_retry_scheduled",
			"count", scheduled, "attempt", attempt, "backoff", backoff.String())
	}
	return stats, nil
}

func (d *OutboxDispatcher) quarantine(ctx context.Context, message OutboxMessage, reason string, stats *DispatchStats) {
	if _, err := d.store.Quarantine(ctx, message.ID, reason); err != nil {
		d.logger.Error("quarantine failed", "outbox_id", message.ID, "error", err)
		return
	}
	stats.Quarantined++
}

// retryDelay is min(base·2^attempt, cap) plus bounded jitter, so retries from
// many replicas do not line up and a short outage does not exhaust the budget.
func (d *OutboxDispatcher) retryDelay(attempt int32) time.Duration {
	delay := d.cfg.Backoff
	for i := int32(0); i < attempt && delay < d.cfg.MaxBackoff; i++ {
		delay *= 2
	}
	if delay > d.cfg.MaxBackoff {
		delay = d.cfg.MaxBackoff
	}
	return delay + time.Duration(rand.Int64N(int64(d.cfg.Backoff)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
