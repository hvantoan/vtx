package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"time"
)

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	// Base middlewares
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(LoggerMiddleware(s.logger))
	r.Use(chimw.Recoverer)

	// CORS configuration for Web client
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:49030", "http://127.0.0.1:49030"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Public Health Probes
	r.Get("/health", s.HandleHealth)
	r.Get("/ready", s.HandleReady)

	// API v1 routes
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/system/status", s.HandleSystemStatus)
		r.Get("/workers", s.HandleWorkers)

		// Authenticated routes
		r.Group(func(r chi.Router) {
			r.Use(BearerAuthMiddleware(s.cfg.AuthToken))
			r.Get("/events", s.HandleSSE)
		})

		// Jira webhook intake: dedicated internal-token group, OUTSIDE the
		// dashboard auth group above. The Nginx gateway injects
		// GATEWAY_INTERNAL_AUTH_TOKEN; the public API token never opens it.
		r.Group(func(r chi.Router) {
			r.Use(chimw.Timeout(10 * time.Second))
			r.Use(GatewayAuthMiddleware(s.cfg.GatewayInternalAuthToken))
			r.Post("/integrations/jira/webhooks", s.HandleJiraWebhook)
		})
		// VTX-3 registry surface. Actor comes from the bearer token (D5), and
		// the group is separate from both the dashboard token and the gateway
		// token: neither opens a registry mutation.
		if s.registryReader != nil && s.registryTransitions != nil {
			r.Group(func(r chi.Router) {
				r.Use(RegistryActorMiddleware(s.cfg.RegistryActorTokens))
				r.Get("/registry/runs/{run_id}", s.HandleRegistryGetRun)
				r.Get("/registry/runs/{run_id}/events", s.HandleRegistryListEvents)
				r.Post("/registry/runs/{run_id}/transitions", s.HandleRegistryTransition)
				r.Get("/registry/tasks/{site_id}/{issue_id}", s.HandleRegistryGetTask)
			})
		}
		// VTX-4 worktree surface. Same actor tokens as the registry — the
		// scheduler (VTX-5) is the expected mutation caller.
		if s.worktree != nil {
			r.Group(func(r chi.Router) {
				r.Use(RegistryActorMiddleware(s.cfg.RegistryActorTokens))
				r.Post("/worktrees/allocate", s.HandleWorktreeAllocate)
				r.Get("/worktrees", s.HandleWorktreeListAllocated)
				r.Get("/worktrees/runs/{run_id}", s.HandleWorktreeGet)
				r.Post("/worktrees/runs/{run_id}/release", s.HandleWorktreeRelease)
			})
		}
	})

	return r
}
