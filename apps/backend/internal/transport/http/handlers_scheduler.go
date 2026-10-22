package http

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/vtx/platform/internal/application"
)

// SchedulerMetricsReader is the slice of the scheduler service the metrics
// endpoint needs. Read-only: admission, release and cancellation are driven
// by the worker/dispatcher, not by HTTP callers in Phase 1.
type SchedulerMetricsReader interface {
	Metrics(ctx context.Context) (application.SchedulerMetrics, error)
}

// WithScheduler wires the VTX-5 scheduler read surface.
func (s *Server) WithScheduler(svc SchedulerMetricsReader) {
	s.scheduler = svc
}

// schedulerMetricsResponse is the wire shape of GET /scheduler/metrics.
type schedulerMetricsResponse struct {
	Waiting           int64 `json:"queue_waiting"`
	Allocated         int64 `json:"queue_allocated"`
	BlockedDependency int64 `json:"blocked_dependency"`
	BlockedUnverified int64 `json:"blocked_unverified"`
	SlotsHeld         int64 `json:"slots_held"`
	SlotsSuspected    int64 `json:"slots_suspected"`
	SlotsMax          int64 `json:"slots_max"`
}

// HandleSchedulerMetrics serves GET /api/v1/scheduler/metrics (AC-10): the
// live queue/active/blocked counters, distinguishing dependency waits from
// heartbeat-suspected slot holds.
func (s *Server) HandleSchedulerMetrics(w http.ResponseWriter, r *http.Request) {
	m, err := s.scheduler.Metrics(r.Context())
	if err != nil {
		http.Error(w, `{"error":"scheduler metrics unavailable"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(schedulerMetricsResponse{
		Waiting:           m.Waiting,
		Allocated:         m.Allocated,
		BlockedDependency: m.BlockedDependency,
		BlockedUnverified: m.BlockedUnverified,
		SlotsHeld:         m.SlotsHeld,
		SlotsSuspected:    m.SlotsSuspected,
		SlotsMax:          m.SlotsMax,
	})
}
