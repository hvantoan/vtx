package http

import (
	"context"
	"fmt"
	"net/http"

	"github.com/vtx/platform/internal/domain"
)

type SSEEventReader interface {
	ListEventsAfter(ctx context.Context, afterID, runID string, limit int32) ([]domain.PlatformEvent, error)
}

func (s *Server) WithSSEEventReader(reader SSEEventReader) *Server {
	s.sseEvents = reader
	return s
}

func (s *Server) sendPersistedSSE(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, afterID, runID string) (string, error) {
	if s.sseEvents == nil {
		return afterID, nil
	}
	events, err := s.sseEvents.ListEventsAfter(ctx, afterID, runID, 200)
	if err != nil {
		return afterID, fmt.Errorf("read SSE events: %w", err)
	}
	for _, event := range events {
		sendSSEEvent(w, flusher, event)
		afterID = event.ID
	}
	return afterID, nil
}
