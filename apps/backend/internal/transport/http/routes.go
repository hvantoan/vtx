package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
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
	})

	return r
}
