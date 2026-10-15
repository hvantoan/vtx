package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/observability"
	transporthttp "github.com/vtx/platform/internal/transport/http"
)

const (
	testGatewayToken = "internal-gateway-token"
	testAPIToken     = "public-dashboard-token"
	webhookPath      = "/api/v1/integrations/jira/webhooks"
)

type stubProcessor struct {
	res application.Result
	err error
	env *domain.JiraWebhookEnvelope
}

func (s *stubProcessor) ProcessWebhookTransition(ctx context.Context, env *domain.JiraWebhookEnvelope, raw []byte, requestID string) (application.Result, error) {
	s.env = env
	return s.res, s.err
}

func setupWebhookServer(t *testing.T, p transporthttp.JiraWebhookProcessor) (http.Handler, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		Port:                     "49080",
		Env:                      "test",
		AuthToken:                testAPIToken,
		GatewayInternalAuthToken: testGatewayToken,
	}
	srv := transporthttp.NewServer(cfg, observability.SetupLogger("error"), nil, nil)
	if p != nil {
		srv.WithJiraWebhook(p)
	}
	return srv.Routes(), cfg
}

func webhookReq(t *testing.T, path, token, contentType string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func validBody(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../integrations/jira/testdata/webhook_valid_transition.json")
	require.NoError(t, err)
	return raw
}

func TestWebhookAuthSeparation(t *testing.T) {
	handler, _ := setupWebhookServer(t, &stubProcessor{res: application.Result{Disposition: application.DispositionAccepted, IntentID: "x"}})
	body := validBody(t)

	t.Run("missing token -> 401", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, webhookReq(t, webhookPath, "", "application/json", body))
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("public API token rejected on webhook route -> 401", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, webhookReq(t, webhookPath, testAPIToken, "application/json", body))
		assert.Equal(t, http.StatusUnauthorized, rr.Code,
			"API_AUTH_TOKEN must NOT open the webhook route — token separation")
	})

	t.Run("internal token accepted", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, webhookReq(t, webhookPath, testGatewayToken, "application/json", body))
		assert.Equal(t, http.StatusAccepted, rr.Code)
	})

	t.Run("dashboard route still requires public token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+testGatewayToken)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusUnauthorized, rr.Code,
			"gateway internal token must NOT open dashboard routes")
	})
}

func TestWebhookIngressControls(t *testing.T) {
	handler, _ := setupWebhookServer(t, &stubProcessor{})
	body := validBody(t)

	t.Run("wrong content type -> 400", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, webhookReq(t, webhookPath, testGatewayToken, "text/plain", body))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("missing content type -> 400", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, webhookReq(t, webhookPath, testGatewayToken, "", body))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("malformed json -> 400", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, webhookReq(t, webhookPath, testGatewayToken, "application/json", []byte("{not json")))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("oversized payload -> 413", func(t *testing.T) {
		big := bytes.Repeat([]byte("x"), 300*1024)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, webhookReq(t, webhookPath, testGatewayToken, "application/json", big))
		assert.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
	})
}

func TestWebhookDispositions(t *testing.T) {
	body := validBody(t)

	cases := []struct {
		name       string
		res        application.Result
		err        error
		wantStatus int
	}{
		{"accepted -> 202 with intent", application.Result{Disposition: application.DispositionAccepted, IntentID: "11111111-1111-1111-1111-111111111111"}, nil, http.StatusAccepted},
		{"duplicate -> 200", application.Result{Disposition: application.DispositionDuplicate, IntentID: "11111111-1111-1111-1111-111111111111"}, nil, http.StatusOK},
		{"ignored_stale -> 202", application.Result{Disposition: application.DispositionIgnoredStale}, nil, http.StatusAccepted},
		{"ignored_scope -> 202", application.Result{Disposition: application.DispositionIgnoredScope}, nil, http.StatusAccepted},
		{"invalid_transition -> 422", application.Result{Disposition: application.DispositionInvalidTransition}, nil, http.StatusUnprocessableEntity},
		{"retryable -> 503", application.Result{}, fmt.Errorf("%w: jira api status 429", jira.ErrRetryable), http.StatusServiceUnavailable},
		{"unexpected error -> 500", application.Result{}, errors.New("db connection lost"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, _ := setupWebhookServer(t, &stubProcessor{res: tc.res, err: tc.err})
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, webhookReq(t, webhookPath, testGatewayToken, "application/json", body))
			assert.Equal(t, tc.wantStatus, rr.Code)

			var resp map[string]any
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			assert.NotEmpty(t, resp["request_id"], "every response carries request_id")
		})
	}
}

func TestWebhookNonStatusFixtureIs202(t *testing.T) {
	// The bulk of Jira traffic: comment/label updates. Must never be 4xx or
	// Jira Cloud auto-disables the webhook.
	handler, _ := setupWebhookServer(t, &stubProcessor{res: application.Result{Disposition: application.DispositionIgnoredScope}})
	raw, err := os.ReadFile("../../integrations/jira/testdata/webhook_no_status_item.json")
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, webhookReq(t, webhookPath, testGatewayToken, "application/json", raw))
	assert.Equal(t, http.StatusAccepted, rr.Code)
	assert.Contains(t, rr.Body.String(), "ignored_scope")
}

func TestWebhookNoSecretsInResponse(t *testing.T) {
	handler, _ := setupWebhookServer(t, &stubProcessor{res: application.Result{Disposition: application.DispositionAccepted, IntentID: "abc"}})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, webhookReq(t, webhookPath, testGatewayToken, "application/json", validBody(t)))
	assert.False(t, strings.Contains(rr.Body.String(), testGatewayToken))
	assert.False(t, strings.Contains(rr.Body.String(), "/webhooks/"))
}
