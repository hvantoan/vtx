package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/domain"
)

var testActorTokens = map[domain.Actor]string{
	domain.ActorScheduler:    "tok-scheduler",
	domain.ActorOrchestrator: "tok-orchestrator",
	domain.ActorOperator:     "tok-operator",
	domain.ActorPolicy:       "tok-policy",
}

type fakeRegistry struct {
	run     application.RunSnapshot
	runView application.RunView
	task    application.TaskView
	events  []application.EventView
	runs    []application.RunView
	err     error
	gotCmd  application.TransitionCommand
	calls   int
	after   *int64
	limit   int32
}

func (f *fakeRegistry) TransitionRun(_ context.Context, cmd application.TransitionCommand) (application.RunSnapshot, error) {
	f.calls++
	f.gotCmd = cmd
	if f.err != nil {
		return application.RunSnapshot{}, f.err
	}
	return f.run, nil
}

func (f *fakeRegistry) GetRun(context.Context, string) (application.RunView, error) {
	if f.err != nil {
		return application.RunView{}, f.err
	}
	return f.runView, nil
}

func (f *fakeRegistry) GetTask(context.Context, string, string) (application.TaskView, error) {
	if f.err != nil {
		return application.TaskView{}, f.err
	}
	return f.task, nil
}

func (f *fakeRegistry) ListEvents(_ context.Context, _ string, afterVersion *int64, limit int32) ([]application.EventView, error) {
	f.after, f.limit = afterVersion, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.events, nil
}

func registryServer(t *testing.T, fake *fakeRegistry) http.Handler {
	t.Helper()
	cfg := &config.Config{AuthToken: "public-token", GatewayInternalAuthToken: "gw-token", RegistryActorTokens: testActorTokens}
	server := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	server.WithRegistry(fake, fake)
	return server.Routes()
}

func doRegistry(t *testing.T, handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestRegistryAuthRejectsMissingAndUnknownTokens: AC-10 — no token and an
// unknown token both 401, and neither reaches the service.
func TestRegistryAuthRejectsMissingAndUnknownTokens(t *testing.T) {
	for _, tc := range []struct{ name, token string }{
		{"missing", ""},
		{"unknown", "not-a-configured-token"},
		{"dashboard token", "public-token"},
		{"gateway token", "gw-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRegistry{}
			rec := doRegistry(t, registryServer(t, fake), http.MethodPost,
				"/api/v1/registry/runs/11111111-1111-1111-1111-111111111111/transitions", tc.token,
				`{"expected_version":0,"action":"cancel"}`)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if fake.calls != 0 {
				t.Errorf("service called %d times, want 0", fake.calls)
			}
		})
	}
}

// TestRegistryActorComesFromTheToken: D5/TR-23 — the actor is the principal the
// token names, not what the body claims.
func TestRegistryActorComesFromTheToken(t *testing.T) {
	fake := &fakeRegistry{run: application.RunSnapshot{RunID: "run-1", State: domain.StateCancelled, Version: 1}}
	rec := doRegistry(t, registryServer(t, fake), http.MethodPost,
		"/api/v1/registry/runs/run-1/transitions", "tok-operator",
		`{"expected_version":0,"action":"cancel","reason":"stop"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if fake.gotCmd.Actor != domain.ActorOperator {
		t.Errorf("actor = %q, want operator from the token", fake.gotCmd.Actor)
	}
	if fake.gotCmd.ActorType != domain.ActorTypeHuman {
		t.Errorf("actor_type = %q, want human for the operator", fake.gotCmd.ActorType)
	}
	if fake.gotCmd.Action != domain.ActionCancel || fake.gotCmd.ExpectedVersion != 0 {
		t.Errorf("command = %+v, want cancel at version 0", fake.gotCmd)
	}
}

// TestRegistryBodyActorCannotImpersonate: a valid token plus a different body
// actor is 403 and never reaches the service.
func TestRegistryBodyActorCannotImpersonate(t *testing.T) {
	fake := &fakeRegistry{}
	rec := doRegistry(t, registryServer(t, fake), http.MethodPost,
		"/api/v1/registry/runs/run-1/transitions", "tok-operator",
		`{"expected_version":0,"action":"cancel","actor":"policy"}`)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if fake.calls != 0 {
		t.Errorf("service called %d times, want 0", fake.calls)
	}
}

// TestRegistryMapsTypedErrorsToStatuses: §0.D error → HTTP mapping.
func TestRegistryMapsTypedErrorsToStatuses(t *testing.T) {
	cases := []struct {
		err  error
		want int
		code string
	}{
		{domain.ErrNotFound, http.StatusNotFound, "not_found"},
		{domain.ErrStaleVersion, http.StatusConflict, "stale_version"},
		{domain.ErrStaleLease, http.StatusConflict, "stale_lease"},
		{domain.ErrAlreadyTerminal, http.StatusConflict, "already_terminal"},
		{domain.ErrInvalidTransition, http.StatusConflict, "invalid_transition"},
		{domain.ErrConflictActiveRun, http.StatusConflict, "conflict_active_run"},
		{domain.ErrStageNotOperational, http.StatusConflict, "stage_not_operational"},
		{domain.ErrForbidden, http.StatusForbidden, "forbidden"},
		{errors.New("boom"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			fake := &fakeRegistry{err: tc.err}
			rec := doRegistry(t, registryServer(t, fake), http.MethodPost,
				"/api/v1/registry/runs/run-1/transitions", "tok-orchestrator",
				`{"expected_version":3,"action":"complete"}`)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			var body registryErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error != tc.code {
				t.Errorf("error = %q, want %q", body.Error, tc.code)
			}
		})
	}
}

// TestRegistryRejectsUnknownActionAndBadBody before any storage call.
func TestRegistryRejectsUnknownActionAndBadBody(t *testing.T) {
	cases := []struct{ name, body, contentType string }{
		{"unknown action", `{"expected_version":0,"action":"teleport"}`, "application/json"},
		{"negative version", `{"expected_version":-1,"action":"cancel"}`, "application/json"},
		{"not json", `{`, "application/json"},
		{"wrong content type", `{"expected_version":0,"action":"cancel"}`, "text/plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRegistry{}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/registry/runs/run-1/transitions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer tok-operator")
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			registryServer(t, fake).ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if fake.calls != 0 {
				t.Errorf("service called %d times, want 0", fake.calls)
			}
		})
	}
}

// TestRegistryEventCursorUsesVersion: TR-23 — cursor is the audit version and
// next_cursor appears only when the page is full.
func TestRegistryEventCursorUsesVersion(t *testing.T) {
	events := make([]application.EventView, 2)
	for i := range events {
		events[i] = application.EventView{
			PreviousState: domain.StateQueued, CurrentState: domain.StatePreparing,
			Action: domain.ActionBegin, Actor: domain.ActorOrchestrator, ActorType: domain.ActorTypeSystem,
			Version: int64(i + 1), CreatedAt: time.Date(2026, 10, 19, 6, 30, 0, 0, time.UTC),
		}
	}
	fake := &fakeRegistry{events: events}
	rec := doRegistry(t, registryServer(t, fake), http.MethodGet,
		"/api/v1/registry/runs/run-1/events?after_version=0&limit=2", "tok-operator", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if fake.after == nil || *fake.after != 0 || fake.limit != 2 {
		t.Errorf("cursor params = (%v,%d), want (0,2)", fake.after, fake.limit)
	}
	var page struct {
		Items      []eventResponse `json:"items"`
		NextCursor *int64          `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	if page.NextCursor == nil || *page.NextCursor != 2 {
		t.Errorf("next_cursor = %v, want 2 (last version)", page.NextCursor)
	}

	// A short page ends the pagination.
	page = struct {
		Items      []eventResponse `json:"items"`
		NextCursor *int64          `json:"next_cursor"`
	}{}
	fake.events = events[:1]
	rec = doRegistry(t, registryServer(t, fake), http.MethodGet,
		"/api/v1/registry/runs/run-1/events?limit=2", "tok-operator", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.NextCursor != nil {
		t.Errorf("next_cursor = %v, want nil on a short page", *page.NextCursor)
	}
}

// TestRegistryReadNotFound: a missing run/task is 404, not 500.
func TestRegistryReadNotFound(t *testing.T) {
	fake := &fakeRegistry{err: application.ErrReadNotFound}
	rec := doRegistry(t, registryServer(t, fake), http.MethodGet, "/api/v1/registry/runs/nope", "tok-operator", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
