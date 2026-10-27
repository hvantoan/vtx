package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
)

type fakeOperatorService struct {
	redrives []application.OperatorAuditRecord
}

func (*fakeOperatorService) Overview(context.Context) (application.OperatorOverview, error) {
	return application.OperatorOverview{}, nil
}
func (*fakeOperatorService) ListRuns(context.Context, application.RunFilter) ([]application.OperatorRunView, error) {
	return nil, nil
}
func (*fakeOperatorService) GetRunDetail(context.Context, string) (application.RunDetailView, error) {
	return application.RunDetailView{}, nil
}
func (*fakeOperatorService) OutboxStats(context.Context) ([]application.OutboxTypeStat, error) {
	return nil, nil
}
func (*fakeOperatorService) ListDeadLettered(context.Context, *string, int32) ([]application.DlqItem, error) {
	return nil, nil
}
func (*fakeOperatorService) Workers(context.Context) ([]application.WorkerHeartbeatView, error) {
	return nil, nil
}
func (f *fakeOperatorService) Redrive(_ context.Context, _ string, a application.OperatorAuditRecord) error {
	f.redrives = append(f.redrives, a)
	return nil
}

func TestSSERejectsCredentialsInQueryEvenWithValidHeader(t *testing.T) {
	h := NewServer(authTestConfig(), discardLogger(), nil, nil).WithAuth(newFakeAuthService()).Routes()
	credentials := []struct {
		name  string
		apply func(*http.Request)
	}{
		{"policy machine token", withBearer("policy-token")},
		{"session cookie", withTestSession},
	}
	for _, credential := range credentials {
		for _, key := range []string{"token", "access_token", "auth", "api_key"} {
			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest(http.MethodGet, "/api/v1/events?"+key+"=leaked", nil).WithContext(ctx)
			credential.apply(req)
			w := &cancelOnFlushRecorder{httptest.NewRecorder(), cancel}
			h.ServeHTTP(w, req)
			cancel()
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "credential_in_query") {
				t.Errorf("%s/%s: status=%d body=%s", credential.name, key, w.Code, w.Body.String())
			}
		}
	}
}

type cancelOnFlushRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *cancelOnFlushRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.cancel()
}

func TestSSEAllowsOnlyOperatorReadCredentials(t *testing.T) {
	h := NewServer(authTestConfig(), discardLogger(), nil, nil).WithAuth(newFakeAuthService()).Routes()
	for _, tc := range []struct {
		name   string
		apply  func(*http.Request)
		status int
	}{
		{"session", withTestSession, http.StatusOK},
		{"policy", withBearer("policy-token"), http.StatusOK},
		{"scheduler", withBearer("scheduler-token"), http.StatusForbidden},
		{"orchestrator", withBearer("orchestrator-token"), http.StatusForbidden},
		{"legacy operator bearer", withBearer("op-token"), http.StatusUnauthorized},
		{"legacy dashboard bearer", withBearer("dashboard-token"), http.StatusUnauthorized},
		{"gateway", withBearer("gateway-token"), http.StatusUnauthorized},
		{"unknown", withBearer("invalid-token"), http.StatusUnauthorized},
		{"missing", nil, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
			if tc.apply != nil {
				tc.apply(req)
			}
			w := &cancelOnFlushRecorder{httptest.NewRecorder(), cancel}
			h.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == http.StatusOK {
				if w.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(w.Body.String(), "event: system.heartbeat") {
					t.Fatalf("expected an SSE heartbeat, got headers=%v body=%s", w.Header(), w.Body.String())
				}
			}
		})
	}
}

func TestOperatorReadEndpointsRequireAllowedActor(t *testing.T) {
	fake := &fakeOperatorService{}
	h := NewServer(authTestConfig(), discardLogger(), nil, nil).
		WithOperator(fake).WithAuth(newFakeAuthService()).Routes()
	paths := []string{
		"/api/v1/operator/overview", "/api/v1/operator/runs", "/api/v1/operator/runs/run-1",
		"/api/v1/operator/outbox", "/api/v1/operator/outbox/dlq", "/api/v1/operator/workers",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			for _, allowed := range []struct {
				name  string
				apply func(*http.Request)
			}{{"session", withTestSession}, {"policy", withBearer("policy-token")}} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				allowed.apply(req)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("%s: status=%d body=%s", allowed.name, w.Code, w.Body.String())
				}
			}
			for _, denied := range []struct {
				name  string
				apply func(*http.Request)
				want  int
			}{
				{"scheduler", withBearer("scheduler-token"), http.StatusForbidden},
				{"legacy operator bearer", withBearer("op-token"), http.StatusUnauthorized},
			} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				denied.apply(req)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != denied.want {
					t.Fatalf("%s: status=%d want=%d body=%s", denied.name, w.Code, denied.want, w.Body.String())
				}
			}
		})
	}
}

func TestOperatorRedriveRequiresOperatorSessionAndRecordsAttribution(t *testing.T) {
	fake := &fakeOperatorService{}
	h := NewServer(authTestConfig(), discardLogger(), nil, nil).
		WithOperator(fake).WithAuth(newFakeAuthService()).Routes()
	path := "/api/v1/operator/outbox/dlq/00000000-0000-0000-0000-000000000001/redrive"

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"retry after fix"}`))
	req.Header.Set("Content-Type", "application/json")
	withBearer("policy-token")(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("policy status=%d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"retry after fix"}`))
	req.Header.Set("Content-Type", "application/json")
	withBearer("op-token")(req)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("legacy operator bearer status=%d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":" retry   after fix "}`))
	req.Header.Set("Content-Type", "application/json")
	// A caller-supplied identity header is ignored: attribution comes from the
	// authenticated session.
	req.Header.Set("X-Operator-Id", "mallory")
	withTestSession(req)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("session status=%d body=%s", w.Code, w.Body.String())
	}
	if len(fake.redrives) != 1 || fake.redrives[0].Actor != string(domain.ActorOperator) ||
		fake.redrives[0].OperatorID != "alice" || fake.redrives[0].Reason != "retry after fix" {
		t.Fatalf("audit=%#v", fake.redrives)
	}
}
