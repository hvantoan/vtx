package http

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"
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

func BearerAuthMiddleware(expectedToken string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var token string
			authHeader := r.Header.Get("Authorization")
			if authHeader != "" {
				parts := strings.SplitN(authHeader, " ", 2)
				if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
					token = parts[1]
				}
			}
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
