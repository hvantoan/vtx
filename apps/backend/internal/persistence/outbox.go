package persistence

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/persistence/db"
)

// PgOutboxStore is the pgx/sqlc implementation of the outbox claim/ack port.
// Every statement is single-statement and idempotent: the dispatcher may crash
// at any point and a replay must be safe.
type PgOutboxStore struct {
	queries *db.Queries
}

func NewPgOutboxStore(queries *db.Queries) *PgOutboxStore {
	return &PgOutboxStore{queries: queries}
}

func (s *PgOutboxStore) ClaimOutbox(ctx context.Context, consumer string, eventTypes []string, limit int32, claimTTL time.Duration) ([]application.OutboxMessage, error) {
	rows, err := s.queries.ClaimOutboxEvents(ctx, db.ClaimOutboxEventsParams{
		Consumer:        consumer,
		ClaimTtlSeconds: int32(claimTTL.Seconds()),
		EventTypes:      eventTypes,
		BatchSize:       limit,
	})
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	messages := make([]application.OutboxMessage, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, application.OutboxMessage{
			ID:        uuidToString(row.OutboxID),
			IntentID:  uuidToString(row.IntentID),
			EventType: row.EventType,
			Payload:   row.Payload,
			Attempt:   row.Attempt,
			CreatedAt: row.CreatedAt.Time,
		})
	}
	return messages, nil
}

func (s *PgOutboxStore) MarkPublished(ctx context.Context, ids []string) (int64, error) {
	parsed, err := uuidSliceFromStrings(ids)
	if err != nil {
		return 0, err
	}
	return s.queries.MarkOutboxPublished(ctx, parsed)
}

func (s *PgOutboxStore) ScheduleRetry(ctx context.Context, ids []string, backoff time.Duration) (int64, error) {
	parsed, err := uuidSliceFromStrings(ids)
	if err != nil {
		return 0, err
	}
	return s.queries.ScheduleOutboxRetry(ctx, db.ScheduleOutboxRetryParams{
		OutboxIds:      parsed,
		BackoffSeconds: int32(backoff.Seconds()),
	})
}

func (s *PgOutboxStore) Quarantine(ctx context.Context, id, reason string) (int64, error) {
	outboxID, err := uuidFromString(id)
	if err != nil {
		return 0, err
	}
	return s.queries.QuarantineOutboxEvent(ctx, db.QuarantineOutboxEventParams{OutboxID: outboxID, Reason: reason})
}

func (s *PgOutboxStore) OutboxLag(ctx context.Context, eventType string) (int64, *time.Time, error) {
	row, err := s.queries.CountOutboxLag(ctx, eventType)
	if err != nil {
		return 0, nil, fmt.Errorf("outbox lag: %w", err)
	}
	if !row.OldestPendingAt.Valid {
		return row.PendingCount, nil, nil
	}
	return row.PendingCount, &row.OldestPendingAt.Time, nil
}

func uuidSliceFromStrings(ids []string) ([]pgtype.UUID, error) {
	parsed := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		value, err := uuidFromString(id)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, value)
	}
	return parsed, nil
}
