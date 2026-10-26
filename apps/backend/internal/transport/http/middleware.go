package http

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vtx/platform/internal/domain"
)

func LoggerMiddleware(logger *slog.Logger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(ww, r)

			// Skip spamming logs for health checks unless error
			if r.URL.Path == "/health" && ww.statusCode == http.StatusOK {
				return
			}

			logger.Info("http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.statusCode),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// RejectQueryCredentials enforces that credentials never appear in URLs,
// which are routinely retained in proxy and browser logs.
func RejectQueryCredentials(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{"token", "access_token", "auth", "api_key"} {
			if _, present := r.URL.Query()[key]; present {
				writeRegistryError(w, http.StatusUnauthorized, "credential_in_query", "credentials must be sent in the Authorization header")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func BearerAuthMiddleware(expectedToken string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, `{"error":"unauthorized","message":"Missing authorization token in Authorization header"}`, http.StatusUnauthorized)
				return
			}

			// Constant-time token comparison (H1 fix)
			if subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) != 1 {
				http.Error(w, `{"error":"unauthorized","message":"Invalid authentication token"}`, http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// EventStreamAuthMiddleware accepts operator reads and legacy dashboard clients.
func EventStreamAuthMiddleware(dashboardToken string, actorTokens map[domain.Actor]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		operatorHandler := RegistryActorMiddleware(actorTokens)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allowOperatorRead(w, r) {
				return
			}
			next.ServeHTTP(w, r)
		}))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(dashboardToken)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			operatorHandler.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the credential from an `Authorization: Bearer <token>`
// header. Empty when absent or malformed.
func bearerToken(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

// GatewayAuthMiddleware guards the Jira webhook intake on the dedicated
// internal token injected by the Nginx gateway — provably separate from the
// public dashboard API_AUTH_TOKEN (asserted at startup in config.Validate).
func GatewayAuthMiddleware(internalToken string) func(next http.Handler) http.Handler {
	return BearerAuthMiddleware(internalToken)
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// registryActorKey is the context key for the authenticated principal. The
// type is unexported so no other package can forge an actor.
type registryActorKey struct{}

// RegistryActorMiddleware authenticates a registry caller and stores the actor
// it may act as. The actor is derived ONLY from the token (D5): a request body
// can never choose its own principal.
//
// Every configured token is compared in constant time before any branch, so the
// response time does not reveal which slot (or whether any slot) matched.
func RegistryActorMiddleware(tokens map[domain.Actor]string) func(next http.Handler) http.Handler {
	// Fixed evaluation order keeps the comparison count independent of input.
	order := []domain.Actor{domain.ActorScheduler, domain.ActorOrchestrator, domain.ActorOperator, domain.ActorPolicy}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				writeRegistryError(w, http.StatusUnauthorized, "unauthorized", "Missing authorization token in Authorization header")
				return
			}

			var actor domain.Actor
			matched := 0
			for _, candidate := range order {
				expected := tokens[candidate]
				if expected == "" {
					continue
				}
				if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
					actor = candidate
					matched++
				}
			}
			// matched > 1 is impossible after ValidateRegistry, but treating it
			// as a rejection keeps a misconfigured deployment fail-closed.
			if matched != 1 {
				writeRegistryError(w, http.StatusUnauthorized, "unauthorized", "Invalid authentication token")
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), registryActorKey{}, actor)))
		})
	}
}

// RegistryActorFromContext returns the authenticated principal.
func RegistryActorFromContext(ctx context.Context) (domain.Actor, bool) {
	actor, ok := ctx.Value(registryActorKey{}).(domain.Actor)
	return actor, ok
}
