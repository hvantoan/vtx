package persistence

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence/db"
)

type PgSSEEventReader struct{ queries *db.Queries }

func NewPgSSEEventReader(queries *db.Queries) *PgSSEEventReader {
	return &PgSSEEventReader{queries: queries}
}
func (s *PgSSEEventReader) ListEventsAfter(ctx context.Context, afterID, runID string, limit int32) ([]domain.PlatformEvent, error) {
	var after pgtype.UUID
	if afterID != "" {
		id, err := uuidFromString(afterID)
		if err != nil {
			return nil, fmt.Errorf("invalid Last-Event-ID: %w", err)
		}
		after = id
	}
	rows, err := s.queries.ListSSEEventsAfter(ctx, db.ListSSEEventsAfterParams{AfterID: after, RunID: textFromOptional(runID), RowLimit: limit})
	if err != nil {
		return nil, fmt.Errorf("list SSE events: %w", err)
	}
	out := make([]domain.PlatformEvent, 0, len(rows))
	for _, row := range rows {
		data := make(map[string]any)
		if err := json.Unmarshal(row.Payload, &data); err != nil {
			return nil, fmt.Errorf("decode SSE event %s: %w", uuidToString(row.OutboxID), err)
		}
		out = append(out, domain.PlatformEvent{ID: uuidToString(row.OutboxID), Type: row.EventType, Timestamp: row.CreatedAt.Time, Source: "vtx.platform.outbox", Data: data})
	}
	return out, nil
}
