package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
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
	cfg := &config.Config{AuthToken: "dashboard-token", RegistryActorTokens: map[domain.Actor]string{}}
	h := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil).Routes()
	for _, key := range []string{"token", "access_token", "auth", "api_key"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?"+key+"=leaked", nil)
		req.Header.Set("Authorization", "Bearer dashboard-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "credential_in_query") {
			t.Errorf("%s: status=%d body=%s", key, w.Code, w.Body.String())
		}
	}
}

func TestOperatorReadEndpointsRequireAllowedActor(t *testing.T) {
	fake := &fakeOperatorService{}
	cfg := &config.Config{AuthToken: "dashboard", RegistryActorTokens: map[domain.Actor]string{
		domain.ActorOperator: "op-token", domain.ActorPolicy: "policy-token", domain.ActorScheduler: "scheduler-token",
	}}
	h := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil).WithOperator(fake).Routes()
	paths := []string{
		"/api/v1/operator/overview", "/api/v1/operator/runs", "/api/v1/operator/runs/run-1",
		"/api/v1/operator/outbox", "/api/v1/operator/outbox/dlq", "/api/v1/operator/workers",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			for _, token := range []string{"op-token", "policy-token"} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("token %s: status=%d body=%s", token, w.Code, w.Body.String())
				}
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer scheduler-token")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("scheduler status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestOperatorRedriveRequiresOperatorAndRecordsAttribution(t *testing.T) {
	fake := &fakeOperatorService{}
	cfg := &config.Config{AuthToken: "dashboard", RegistryActorTokens: map[domain.Actor]string{domain.ActorOperator: "op-token", domain.ActorPolicy: "policy-token"}}
	h := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil).WithOperator(fake).Routes()
	path := "/api/v1/operator/outbox/dlq/00000000-0000-0000-0000-000000000001/redrive"
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":" retry after fix "}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer policy-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("policy status=%d", w.Code)
	}
	req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":" retry   after fix "}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer op-token")
	req.Header.Set("X-Operator-Id", " alice   smith ")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("operator status=%d body=%s", w.Code, w.Body.String())
	}
	if len(fake.redrives) != 1 || fake.redrives[0].Actor != "operator" || fake.redrives[0].OperatorID != "alice smith" || fake.redrives[0].Reason != "retry after fix" {
		t.Fatalf("audit=%#v", fake.redrives)
	}
}
