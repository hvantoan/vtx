package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/integrations/jira"
)

const maxWebhookBody = 256 * 1024 // 256 KiB ingress limit (spec §3.3)

// JiraWebhookProcessor is the application-service contract the handler needs.
type JiraWebhookProcessor interface {
	ProcessWebhookTransition(ctx context.Context, env *domain.JiraWebhookEnvelope, rawPayload []byte, requestID string) (application.Result, error)
}

type webhookResponse struct {
	RequestID   string `json:"request_id"`
	Disposition string `json:"disposition"`
	IntentID    string `json:"intent_id,omitempty"`
}

type webhookErrorResponse struct {
	RequestID string `json:"request_id"`
	Error     string `json:"error"`
	Message   string `json:"message"`
}

// WithJiraWebhook binds the application service. Kept out of NewServer's
// signature so the dashboard-only bootstrap and existing tests stay valid;
// the route is only registered when a processor is present.
func (s *Server) WithJiraWebhook(p JiraWebhookProcessor) *Server {
	s.jiraWebhook = p
	return s
}

// HandleJiraWebhook serves POST /api/v1/integrations/jira/webhooks.
//
// Ingress order is deliberate: auth (middleware) → Content-Type → size →
// JSON parse → service. Ignorable Jira traffic returns 202 (never 4xx) so
// sustained non-status events cannot get the webhook auto-disabled; 422 is
// reserved for structurally-accepted-but-unprocessable envelopes; retryable
// upstream failures return 503 so Jira redelivers.
func (s *Server) HandleJiraWebhook(w http.ResponseWriter, r *http.Request) {
	reqID := chimw.GetReqID(r.Context())

	if s.jiraWebhook == nil {
		writeWebhookError(w, reqID, http.StatusServiceUnavailable, "unavailable", "webhook intake not configured")
		return
	}

	ct := r.Header.Get("Content-Type")

	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
		writeWebhookError(w, reqID, http.StatusBadRequest, "bad_request", "Content-Type must be application/json")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	raw, err := readAllLimited(r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeWebhookError(w, reqID, http.StatusRequestEntityTooLarge, "payload_too_large", "payload exceeds 256 KiB")
			return
		}
		writeWebhookError(w, reqID, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}

	var env domain.JiraWebhookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		writeWebhookError(w, reqID, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}

	res, err := s.jiraWebhook.ProcessWebhookTransition(r.Context(), &env, raw, reqID)
	if err != nil {
		if errors.Is(err, jira.ErrRetryable) {
			s.logger.Warn("jira webhook retryable failure",
				slog.String("request_id", reqID))
			writeWebhookError(w, reqID, http.StatusServiceUnavailable, "unavailable", "transient upstream failure — retry via Jira redelivery")
			return
		}
		s.logger.Error("jira webhook processing failed",
			slog.String("request_id", reqID),
			slog.String("error", err.Error()))
		writeWebhookError(w, reqID, http.StatusInternalServerError, "internal_error", "processing failed")
		return
	}

	// Outcome is logged, never the request path (the gateway already stripped
	// the secret) and never the payload body.
	s.logger.Info("jira webhook processed",
		slog.String("request_id", reqID),
		slog.String("disposition", res.Disposition))
	status := http.StatusAccepted
	switch res.Disposition {
	case application.DispositionDuplicate:
		status = http.StatusOK
	case application.DispositionInvalidTransition:
		status = http.StatusUnprocessableEntity
	}
	writeWebhookResponse(w, reqID, status, res)
}

func readAllLimited(r *http.Request) ([]byte, error) {
	// io.ReadAll honours MaxBytesReader's limit error (*http.MaxBytesError).
	return io.ReadAll(r.Body)
}

func writeWebhookResponse(w http.ResponseWriter, reqID string, status int, res application.Result) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(webhookResponse{
		RequestID:   reqID,
		Disposition: res.Disposition,
		IntentID:    res.IntentID,
	})
}

func writeWebhookError(w http.ResponseWriter, reqID string, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(webhookErrorResponse{
		RequestID: reqID,
		Error:     code,
		Message:   message,
	})
}
