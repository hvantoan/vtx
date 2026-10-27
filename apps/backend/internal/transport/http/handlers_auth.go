package http

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/vtx/platform/internal/application"
)

const sessionCookieName = "vtx_session"

// AuthService is the app-hosted login surface. Implemented by
// application.AuthService.
type AuthService interface {
	Login(ctx context.Context, username, password, clientKey string) (application.LoginResult, error)
	Authenticate(ctx context.Context, token string) (application.AuthSession, error)
	Logout(ctx context.Context, token string) error
	ChangePassword(ctx context.Context, session application.AuthSession, currentPassword, newPassword string) error
}

func (s *Server) WithAuth(v AuthService) *Server { s.auth = v; return s }

type authSessionResponse struct {
	Username  string `json:"username"`
	ExpiresAt string `json:"expires_at"`
}

func (s *Server) HandleAuthLogin(w http.ResponseWriter, r *http.Request) {
	body, err := readLimitedJSON(w, r)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.Unmarshal(body, &req) != nil {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", "body is not valid JSON")
		return
	}

	result, err := s.auth.Login(r.Context(), req.Username, req.Password, loginClientKey(r))
	switch {
	case errors.Is(err, application.ErrTooManyAttempts):
		w.Header().Set("Retry-After", "900")
		writeRegistryError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed login attempts")
	case errors.Is(err, application.ErrInvalidCredentials):
		writeRegistryError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
	case err != nil:
		s.logger.Error("auth login failed", "error", err)
		writeRegistryError(w, http.StatusInternalServerError, "internal_error", "internal error")
	default:
		s.setSessionCookie(w, r, result.Token, result.ExpiresAt)
		writeRegistryJSON(w, http.StatusOK, authSessionResponse{
			Username:  result.Session.Username,
			ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339),
		})
	}
}

func (s *Server) HandleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		if err := s.auth.Logout(r.Context(), cookie.Value); err != nil {
			s.logger.Error("auth logout failed", "error", err)
			writeRegistryError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
	}
	s.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) HandleAuthMe(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	writeRegistryJSON(w, http.StatusOK, authSessionResponse{
		Username:  session.Username,
		ExpiresAt: session.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) HandleAuthPassword(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	body, err := readLimitedJSON(w, r)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if json.Unmarshal(body, &req) != nil {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", "body is not valid JSON")
		return
	}

	err = s.auth.ChangePassword(r.Context(), session, req.CurrentPassword, req.NewPassword)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, application.ErrInvalidCredentials):
		writeRegistryError(w, http.StatusUnauthorized, "invalid_credentials", "current password is incorrect")
	case errors.Is(err, application.ErrWeakPassword):
		writeRegistryError(w, http.StatusBadRequest, "weak_password", err.Error())
	case errors.Is(err, application.ErrSessionInvalid):
		writeRegistryError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
	default:
		s.logger.Error("auth password change failed", "error", err)
		writeRegistryError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (application.AuthSession, bool) {
	if s.auth == nil {
		writeRegistryError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return application.AuthSession{}, false
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		writeRegistryError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return application.AuthSession{}, false
	}
	session, err := s.auth.Authenticate(r.Context(), cookie.Value)
	switch {
	case err == nil:
		return session, true
	case errors.Is(err, application.ErrSessionInvalid):
		writeRegistryError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
	default:
		s.logger.Error("session authentication failed", "error", err)
		writeRegistryError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
	return application.AuthSession{}, false
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
		MaxAge:   maxAge,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
	})
}

// secureCookie resolves SESSION_COOKIE_SECURE. "auto" marks the cookie Secure
// when the request arrived over TLS, including a TLS-terminating proxy in front
// (the tunnel) — verified by X-Forwarded-Proto.
func (s *Server) secureCookie(r *http.Request) bool {
	switch strings.ToLower(s.cfg.SessionCookieSecure) {
	case "always":
		return true
	case "never":
		return false
	default:
		if r.TLS != nil {
			return true
		}
		proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
		return strings.EqualFold(proto, "https")
	}
}

// loginClientKey is the throttling identity: the client address, never the
// username alone (one account exists) and never a header the caller can set.
func loginClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
