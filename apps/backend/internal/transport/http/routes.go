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

	// CORS configuration for the local web dev client. Deployments serve the
	// dashboard same-origin through the Next.js /api proxy, so no other origin
	// needs credentialed access.
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

		// App-hosted operator login. These routes exist only when the API has a
		// session store; without a database nothing can be authenticated.
		if s.auth != nil {
			r.Route("/auth", func(r chi.Router) {
				r.Use(chimw.Timeout(15 * time.Second))
				r.Post("/login", s.HandleAuthLogin)
				r.Post("/logout", s.HandleAuthLogout)
				r.Get("/me", s.HandleAuthMe)
				r.Post("/password", s.HandleAuthPassword)
			})
		}

		// Authenticated routes
		r.Group(func(r chi.Router) {
			r.Use(RejectQueryCredentials)
			r.Use(s.streamAuthMiddleware)
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
				r.Use(s.actorMiddleware)
				r.Get("/registry/runs/{run_id}", s.HandleRegistryGetRun)
				r.Get("/registry/runs/{run_id}/events", s.HandleRegistryListEvents)
				r.Post("/registry/runs/{run_id}/transitions", s.HandleRegistryTransition)
				r.Get("/registry/tasks/{site_id}/{issue_id}", s.HandleRegistryGetTask)
			})
		}
		if s.operator != nil {
			r.Group(func(r chi.Router) {
				r.Use(s.actorMiddleware)
				r.Get("/operator/overview", s.HandleOperatorOverview)
				r.Get("/operator/runs", s.HandleOperatorRuns)
				r.Get("/operator/runs/{run_id}", s.HandleOperatorRunDetail)
				r.Get("/operator/outbox", s.HandleOperatorOutbox)
				r.Get("/operator/outbox/dlq", s.HandleOperatorDLQ)
				r.Post("/operator/outbox/dlq/{outbox_id}/redrive", s.HandleOperatorRedrive)
				r.Get("/operator/workers", s.HandleOperatorWorkers)
			})
		}
		if s.pullRequests != nil {
			r.Group(func(r chi.Router) {
				r.Use(s.actorMiddleware)
				r.Get("/registry/runs/{run_id}/pull_request", s.HandleGetRunPullRequest)
				r.Get("/pull_requests", s.HandleListPullRequests)
				r.Post("/pull_requests/{run_id}/sync", s.HandleSyncPullRequest)
			})
		}
		// VTX-4 worktree surface. Same actor tokens as the registry — the
		// scheduler (VTX-5) is the expected mutation caller.
		if s.worktree != nil {
			r.Group(func(r chi.Router) {
				r.Use(s.actorMiddleware)
				r.Post("/worktrees/allocate", s.HandleWorktreeAllocate)
				r.Get("/worktrees", s.HandleWorktreeListAllocated)
				r.Get("/worktrees/runs/{run_id}", s.HandleWorktreeGet)
				r.Post("/worktrees/runs/{run_id}/release", s.HandleWorktreeRelease)
			})
		}
	})

	s.qualityRoutes(r)
	return r
}
