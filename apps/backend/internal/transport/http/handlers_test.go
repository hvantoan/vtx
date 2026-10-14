package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/observability"
	transporthttp "github.com/vtx/platform/internal/transport/http"
)

type mockDependency struct {
	latency time.Duration
	err     error
}

func (m *mockDependency) Ping(ctx context.Context) (time.Duration, error) {
	return m.latency, m.err
}

func setupTestServer(db transporthttp.DependencyChecker, temporal transporthttp.DependencyChecker) (http.Handler, *config.Config) {
	cfg := &config.Config{
		Port:          "8080",
		Env:           "test",
		AuthToken:     "test-secret-token",
		TemporalQueue: "test-queue",
	}
	logger := observability.SetupLogger("error")
	srv := transporthttp.NewServer(cfg, logger, db, temporal)
	return srv.Routes(), cfg
}

func TestHealthHandler(t *testing.T) {
	handler, _ := setupTestServer(nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	var resp domain.HealthStatus
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Status)
	assert.True(t, resp.UptimeSeconds >= 0)
}

func TestReadyHandler_AllReady(t *testing.T) {
	dbMock := &mockDependency{latency: 2 * time.Millisecond, err: nil}
	temporalMock := &mockDependency{latency: 5 * time.Millisecond, err: nil}
	handler, _ := setupTestServer(dbMock, temporalMock)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp domain.ReadyStatus
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "ready", resp.Status)
	assert.Equal(t, "up", resp.Dependencies["database"].Status)
	assert.Equal(t, "up", resp.Dependencies["temporal"].Status)
}

func TestReadyHandler_DependencyDown(t *testing.T) {
	dbMock := &mockDependency{latency: 0, err: errors.New("connection refused")}
	temporalMock := &mockDependency{latency: 5 * time.Millisecond, err: nil}
	handler, _ := setupTestServer(dbMock, temporalMock)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)

	var resp domain.ReadyStatus
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "not_ready", resp.Status)
	assert.Equal(t, "down", resp.Dependencies["database"].Status)
	assert.NotNil(t, resp.Dependencies["database"].Error)
	assert.Equal(t, "up", resp.Dependencies["temporal"].Status)
}

func TestSystemStatusHandler(t *testing.T) {
	handler, _ := setupTestServer(nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp domain.SystemStatus
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", resp.Version)
	assert.Equal(t, "test", resp.Environment)
	assert.True(t, resp.Goroutines > 0)
}

func TestSSEAuth_Unauthorized(t *testing.T) {
	handler, _ := setupTestServer(nil, nil)

	// No token
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)

	// Wrong token
	req = httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestSSE_AuthenticatedSuccess(t *testing.T) {
	handler, cfg := setupTestServer(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "text/event-stream", rr.Header().Get("Content-Type"))
	assert.Contains(t, rr.Body.String(), "system.heartbeat")
}

func TestWorkersHandler(t *testing.T) {
	handler, _ := setupTestServer(nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp domain.WorkerStatusList
	err := json.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "healthy", resp.Status)
	assert.Len(t, resp.Workers, 1)
}
