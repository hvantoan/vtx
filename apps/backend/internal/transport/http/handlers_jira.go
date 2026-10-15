package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/integrations/jira"
)

const maxWebhookBody = 256 * 1024 // 256 KiB ingress limit (spec §3.3)

// JiraWebhookProcessor is the application-service contract the handler needs.
// receivedAt is captured at handler entry (before body parse) — the source of
// truth for receipt audit rows and the occurred_at fallback.
type JiraWebhookProcessor interface {
	ProcessWebhookTransition(ctx context.Context, env *domain.JiraWebhookEnvelope, rawPayload []byte, requestID string, receivedAt time.Time) (application.Result, error)
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

// ReceiptRecorder is the audit sink for authenticated webhook requests —
// satisfied by application.ReceiptRecorder. Optional: tests may leave it nil.
type ReceiptRecorder interface {
	Record(ctx context.Context, rec application.Receipt) error
}

// WithJiraWebhook binds the application service. Kept out of NewServer's
// signature so the dashboard-only bootstrap and existing tests stay valid;
// the route is only registered when a processor is present.
func (s *Server) WithJiraWebhook(p JiraWebhookProcessor) *Server {
	s.jiraWebhook = p
	return s
}

// WithJiraReceipts binds the append-only receipt recorder (review P2-1).
func (s *Server) WithJiraReceipts(rec ReceiptRecorder) *Server {
	s.jiraReceipts = rec
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
	// receivedAt stamps the request at handler entry — receipt audit rows and
	// the occurred_at fallback both hang off this single value.
	receivedAt := time.Now().UTC()

	// Receipt audit (review P2-1): every request that got past
	// GatewayAuthMiddleware settles into exactly one append-only row.
	// Fields accumulate as ingress progresses — transport rejects have no
	// identity, parsed envelopes do. Best-effort: a failed insert logs an
	// error and never changes the HTTP response.
	rec := application.Receipt{ReceivedAt: receivedAt, RequestID: reqID}
	defer func() {
		if s.jiraReceipts == nil || rec.Outcome == "" {
			return
		}
		if err := s.jiraReceipts.Record(r.Context(), rec); err != nil {
			s.logger.Error("webhook receipt write failed",
				slog.String("request_id", reqID),
				slog.String("error", err.Error()))
		}
	}()

	if s.jiraWebhook == nil {
		rec.Outcome = application.DispositionRejected
		rec.ReasonCode = application.ReasonInternalError
		writeWebhookError(w, reqID, http.StatusServiceUnavailable, "unavailable", "webhook intake not configured")
		return
	}

	ct := r.Header.Get("Content-Type")

	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
		rec.Outcome = application.DispositionRejected
		rec.ReasonCode = application.ReasonBadContentType
		writeWebhookError(w, reqID, http.StatusBadRequest, "bad_request", "Content-Type must be application/json")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	raw, err := readAllLimited(r)
	// payload_hash hashes what was read — an oversized body hashes the
	// truncated prefix; the hash is diagnostic, not a completeness proof.
	rec.PayloadHash = hashHex(raw)
	if err != nil {
		var mbe *http.MaxBytesError
		rec.Outcome = application.DispositionRejected
		if errors.As(err, &mbe) {
			rec.ReasonCode = application.ReasonPayloadTooLarge
			writeWebhookError(w, reqID, http.StatusRequestEntityTooLarge, "payload_too_large", "payload exceeds 256 KiB")
			return
		}
		rec.ReasonCode = application.ReasonBodyReadFailure
		rec.ErrorDetail = "body read failed"
		writeWebhookError(w, reqID, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}

	var env domain.JiraWebhookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		rec.Outcome = application.DispositionRejected
		rec.ReasonCode = application.ReasonMalformedJSON
		writeWebhookError(w, reqID, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if env.Issue != nil {
		rec.IssueKey = env.Issue.Key
	}

	res, err := s.jiraWebhook.ProcessWebhookTransition(r.Context(), &env, raw, reqID, receivedAt)
	if err != nil {
		if errors.Is(err, jira.ErrRetryable) {
			rec.Outcome = application.DispositionFailedRetryable
			rec.ErrorDetail = "transient upstream failure"
			s.logger.Warn("jira webhook retryable failure",
				slog.String("request_id", reqID))
			writeWebhookError(w, reqID, http.StatusServiceUnavailable, "unavailable", "transient upstream failure — retry via Jira redelivery")
			return
		}
		rec.Outcome = application.DispositionRejected
		rec.ReasonCode = application.ReasonInternalError
		rec.ErrorDetail = "processing failed"
		s.logger.Error("jira webhook processing failed",
			slog.String("request_id", reqID),
			slog.String("error", err.Error()))
		writeWebhookError(w, reqID, http.StatusInternalServerError, "internal_error", "processing failed")
		return
	}

	rec.Outcome = res.Disposition
	rec.ReasonCode = res.ReasonCode

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

func hashHex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
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
