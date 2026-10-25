package http

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/gate"
	"github.com/vtx/platform/internal/qa"
)

type QAService interface {
	LaunchSession(context.Context, qa.LaunchCommand) (qa.Session, error)
	SubmitVerdict(context.Context, qa.SubmitCommand) (qa.VerdictRecord, error)
	VerdictForSHA(context.Context, string, string) (*qa.VerdictRecord, error)
	ListEvidence(context.Context, string, string) ([]qa.EvidenceArtifact, error)
}
type GateService interface {
	Evaluate(context.Context, string, int64) (gate.Decision, error)
	LatestDecision(context.Context, string, string) (*gate.Decision, error)
	History(context.Context, string) ([]gate.Decision, error)
}
type DeliveryRedriver interface {
	RedriveDelivery(context.Context, string, string, string, string, string) (bool, error)
}

func (s *Server) WithQuality(qaService QAService, gateService GateService, redriver DeliveryRedriver, qaToken string) *Server {
	s.qa = qaService
	s.gate = gateService
	s.redriver = redriver
	s.qaToken = qaToken
	return s
}
func (s *Server) qualityRoutes(r chi.Router) {
	r.Route("/internal/v1", func(r chi.Router) {
		// QA submission has a dedicated credential, never a transition actor.
		if s.qa != nil {
			r.With(GatewayAuthMiddleware(s.qaToken)).Post("/runs/{run_id}/qa/verdict", s.HandleQASubmit)
		}
		r.Group(func(r chi.Router) {
			r.Use(RegistryActorMiddleware(s.cfg.RegistryActorTokens))
			if s.qa != nil {
				r.Post("/runs/{run_id}/qa/session", s.HandleQALaunch)
				r.Get("/runs/{run_id}/qa/verdict", s.HandleQARead)
				r.Get("/runs/{run_id}/evidence", s.HandleEvidenceRead)
			}
			if s.gate != nil {
				r.Post("/runs/{run_id}/gate/evaluate", s.HandleGateEvaluate)
				r.Get("/runs/{run_id}/gate/decision", s.HandleGateRead)
				r.Get("/runs/{run_id}/gate/history", s.HandleGateHistory)
			}
			if s.redriver != nil {
				// Internal per-consumer redrive (VTX-11 audit: delivery_redrives).
				// The public operator path POST /api/v1/operator/outbox/dlq/{id}/redrive
				// is owned by the operator group in routes.go.
				r.Post("/outbox/dlq/{outbox_id}/redrive", s.HandleDeliveryRedrive)
			}
		})
	})
}
func qualityActor(w http.ResponseWriter, r *http.Request, allowed ...domain.Actor) bool {
	actor, ok := RegistryActorFromContext(r.Context())
	if ok {
		for _, a := range allowed {
			if actor == a {
				return true
			}
		}
	}
	writeRegistryError(w, http.StatusForbidden, "forbidden", "principal cannot perform this action")
	return false
}
func qualityBody(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, err := readLimitedJSON(w, r)
	if err != nil {
		writeRegistryError(w, 400, "invalid_request", err.Error())
		return false
	}
	if err := json.Unmarshal(raw, target); err != nil {
		writeRegistryError(w, 400, "invalid_request", "invalid JSON body")
		return false
	}
	return true
}
func qualitySHA(w http.ResponseWriter, r *http.Request) (string, bool) {
	sha := r.URL.Query().Get("sha")
	if sha == "" {
		writeRegistryError(w, 400, "invalid_request", "sha is required")
		return "", false
	}
	return sha, true
}
func (s *Server) HandleQALaunch(w http.ResponseWriter, r *http.Request) {
	if !qualityActor(w, r, domain.ActorOrchestrator) {
		return
	}
	var c qa.LaunchCommand
	if !qualityBody(w, r, &c) {
		return
	}
	c.RunID = chi.URLParam(r, "run_id")
	v, err := s.qa.LaunchSession(r.Context(), c)
	if err != nil {
		s.writeRegistryServiceError(w, err)
		return
	}
	writeRegistryJSON(w, 200, v)
}
func (s *Server) HandleQASubmit(w http.ResponseWriter, r *http.Request) {
	var c qa.SubmitCommand
	if !qualityBody(w, r, &c) {
		return
	}
	c.RunID = chi.URLParam(r, "run_id")
	c.QAIdentity = "qa-agent"
	v, err := s.qa.SubmitVerdict(r.Context(), c)
	if err != nil {
		s.writeRegistryServiceError(w, err)
		return
	}
	writeRegistryJSON(w, 200, v)
}
func (s *Server) HandleQARead(w http.ResponseWriter, r *http.Request) {
	if !qualityActor(w, r, domain.ActorOperator, domain.ActorOrchestrator) {
		return
	}
	sha, ok := qualitySHA(w, r)
	if !ok {
		return
	}
	v, err := s.qa.VerdictForSHA(r.Context(), chi.URLParam(r, "run_id"), sha)
	if err != nil {
		s.writeRegistryServiceError(w, err)
		return
	}
	writeRegistryJSON(w, 200, v)
}
func (s *Server) HandleEvidenceRead(w http.ResponseWriter, r *http.Request) {
	if !qualityActor(w, r, domain.ActorOperator, domain.ActorOrchestrator) {
		return
	}
	sha, ok := qualitySHA(w, r)
	if !ok {
		return
	}
	v, err := s.qa.ListEvidence(r.Context(), chi.URLParam(r, "run_id"), sha)
	if err != nil {
		s.writeRegistryServiceError(w, err)
		return
	}
	writeRegistryJSON(w, 200, v)
}
func (s *Server) HandleGateEvaluate(w http.ResponseWriter, r *http.Request) {
	if !qualityActor(w, r, domain.ActorOrchestrator, domain.ActorPolicy) {
		return
	}
	var c struct {
		LeaseEpoch int64 `json:"lease_epoch"`
	}
	if !qualityBody(w, r, &c) {
		return
	}
	v, err := s.gate.Evaluate(r.Context(), chi.URLParam(r, "run_id"), c.LeaseEpoch)
	if err != nil {
		s.writeRegistryServiceError(w, err)
		return
	}
	writeRegistryJSON(w, 200, v)
}
func (s *Server) HandleGateRead(w http.ResponseWriter, r *http.Request) {
	if !qualityActor(w, r, domain.ActorOperator, domain.ActorOrchestrator) {
		return
	}
	sha, ok := qualitySHA(w, r)
	if !ok {
		return
	}
	v, err := s.gate.LatestDecision(r.Context(), chi.URLParam(r, "run_id"), sha)
	if err != nil {
		s.writeRegistryServiceError(w, err)
		return
	}
	writeRegistryJSON(w, 200, v)
}
func (s *Server) HandleGateHistory(w http.ResponseWriter, r *http.Request) {
	if !qualityActor(w, r, domain.ActorOperator) {
		return
	}
	v, err := s.gate.History(r.Context(), chi.URLParam(r, "run_id"))
	if err != nil {
		s.writeRegistryServiceError(w, err)
		return
	}
	writeRegistryJSON(w, 200, v)
}
func (s *Server) HandleDeliveryRedrive(w http.ResponseWriter, r *http.Request) {
	if !qualityActor(w, r, domain.ActorOperator) {
		return
	}
	var c struct {
		Consumer      string `json:"consumer_name"`
		Reason        string `json:"reason"`
		CorrelationID string `json:"correlation_id"`
	}
	if !qualityBody(w, r, &c) {
		return
	}
	changed, err := s.redriver.RedriveDelivery(r.Context(), chi.URLParam(r, "outbox_id"), c.Consumer, "operator", c.Reason, c.CorrelationID)
	if err != nil {
		s.writeRegistryServiceError(w, err)
		return
	}
	writeRegistryJSON(w, 200, map[string]bool{"redriven": changed})
}
