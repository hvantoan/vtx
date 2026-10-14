package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/domain"
)

type DependencyChecker interface {
	Ping(ctx context.Context) (time.Duration, error)
}

type Server struct {
	cfg        *config.Config
	logger     *slog.Logger
	startTime  time.Time
	db         DependencyChecker
	temporal   DependencyChecker
	sseClients map[chan domain.PlatformEvent]struct{}
	sseMu      sync.RWMutex
}

func NewServer(cfg *config.Config, logger *slog.Logger, db DependencyChecker, temporal DependencyChecker) *Server {
	return &Server{
		cfg:        cfg,
		logger:     logger,
		startTime:  time.Now(),
		db:         db,
		temporal:   temporal,
		sseClients: make(map[chan domain.PlatformEvent]struct{}),
	}
}

// HandleHealth serves GET /health (Liveness probe)
func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.startTime).Seconds()
	resp := domain.HealthStatus{
		Status:        "ok",
		Timestamp:     time.Now().UTC(),
		UptimeSeconds: uptime,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleReady serves GET /ready (Readiness probe)
func (s *Server) HandleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	deps := make(map[string]domain.DependencyStatus)
	allReady := true

	// Check Database
	if s.db != nil {
		latency, err := s.db.Ping(ctx)
		status := "up"
		var errStr *string
		if err != nil {
			status = "down"
			allReady = false
			e := err.Error()
			errStr = &e
		}
		deps["database"] = domain.DependencyStatus{
			Status:    status,
			LatencyMS: float64(latency.Milliseconds()),
			Error:     errStr,
		}
	} else {
		errMsg := "database client unconfigured"
		deps["database"] = domain.DependencyStatus{
			Status:    "down",
			LatencyMS: 0,
			Error:     &errMsg,
		}
		allReady = false
	}

	// Check Temporal
	if s.temporal != nil {
		latency, err := s.temporal.Ping(ctx)
		status := "up"
		var errStr *string
		if err != nil {
			status = "down"
			allReady = false
			e := err.Error()
			errStr = &e
		}
		deps["temporal"] = domain.DependencyStatus{
			Status:    status,
			LatencyMS: float64(latency.Milliseconds()),
			Error:     errStr,
		}
	} else {
		// In standalone or test mode without temporal client, mark degraded or down
		status := "down"
		errMsg := "temporal client unconfigured"
		deps["temporal"] = domain.DependencyStatus{
			Status:    status,
			LatencyMS: 0,
			Error:     &errMsg,
		}
		allReady = false
	}

	overallStatus := "ready"
	httpCode := http.StatusOK
	if !allReady {
		overallStatus = "not_ready"
		httpCode = http.StatusServiceUnavailable
	}

	resp := domain.ReadyStatus{
		Status:       overallStatus,
		Timestamp:    time.Now().UTC(),
		Dependencies: deps,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpCode)
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleSystemStatus serves GET /api/v1/system/status
func (s *Server) HandleSystemStatus(w http.ResponseWriter, r *http.Request) {
	resp := domain.SystemStatus{
		Version:       "1.0.0",
		Commit:        "dev-bootstrap",
		Environment:   s.cfg.Env,
		GoVersion:     runtime.Version(),
		Goroutines:    runtime.NumGoroutine(),
		UptimeSeconds: time.Since(s.startTime).Seconds(),
		Timestamp:     time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleWorkers serves GET /api/v1/workers
func (s *Server) HandleWorkers(w http.ResponseWriter, r *http.Request) {
	resp := domain.WorkerStatusList{
		Status: "healthy",
		Workers: []domain.WorkerItem{
			{
				ID:              "temporal-worker-vtx-1",
				Queue:           s.cfg.TemporalQueue,
				Status:          "running",
				ActiveWorkflows: 0,
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleSSE serves GET /api/v1/events (Authenticated SSE stream)
func (s *Server) HandleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	eventCh := make(chan domain.PlatformEvent, 16)
	s.sseMu.Lock()
	s.sseClients[eventCh] = struct{}{}
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.sseClients, eventCh)
		close(eventCh)
		s.sseMu.Unlock()
	}()

	// Send initial connected event
	initialEvent := domain.PlatformEvent{
		ID:        fmt.Sprintf("conn-%d", time.Now().UnixNano()),
		Type:      "system.heartbeat",
		Timestamp: time.Now().UTC(),
		Source:    "vtx.platform.api",
		Data: map[string]any{
			"message": "SSE connection established successfully",
			"uptime":  time.Since(s.startTime).Seconds(),
		},
	}
	sendSSEEvent(w, flusher, initialEvent)

	heartbeatTicker := time.NewTicker(5 * time.Second)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-eventCh:
			sendSSEEvent(w, flusher, event)
		case t := <-heartbeatTicker.C:
			hbEvent := domain.PlatformEvent{
				ID:        fmt.Sprintf("hb-%d", t.UnixNano()),
				Type:      "system.heartbeat",
				Timestamp: t.UTC(),
				Source:    "vtx.platform.api",
				Data: map[string]any{
					"uptime":     time.Since(s.startTime).Seconds(),
					"goroutines": runtime.NumGoroutine(),
				},
			}
			sendSSEEvent(w, flusher, hbEvent)
		}
	}
}

func (s *Server) BroadcastEvent(event domain.PlatformEvent) {
	s.sseMu.RLock()
	defer s.sseMu.RUnlock()
	for ch := range s.sseClients {
		select {
		case ch <- event:
		default:
		}
	}
}

func sendSSEEvent(w http.ResponseWriter, flusher http.Flusher, event domain.PlatformEvent) {
	dataBytes, _ := json.Marshal(event)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(dataBytes))
	flusher.Flush()
}
