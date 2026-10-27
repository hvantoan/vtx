package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/domain"
)

const (
	testSessionValue = "test-session-value"
	// testOperatorSession is the credential marker the shared helpers translate
	// into a session cookie (the operator no longer has a bearer token).
	testOperatorSession = "session:operator"
)

type loginCall struct {
	username, password, clientKey string
}

type changePasswordCall struct {
	current, next string
}

type fakeAuthService struct {
	session       application.AuthSession
	loginResult   application.LoginResult
	loginErr      error
	logoutErr     error
	changeErr     error
	lastLogin     loginCall
	lastChange    changePasswordCall
	lastChangeFor application.AuthSession
	loggedOut     []string
}

func newFakeAuthService() *fakeAuthService {
	f := &fakeAuthService{}
	return f.sessionDefaults()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (f *fakeAuthService) sessionDefaults() *fakeAuthService {
	f.session = application.AuthSession{
		SessionID:  "session-1",
		UserID:     "user-1",
		Username:   "alice",
		LastSeenAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC),
	}
	f.loginResult = application.LoginResult{
		Session:   f.session,
		Token:     "minted-session-token",
		ExpiresAt: f.session.ExpiresAt,
	}
	return f
}

func (f *fakeAuthService) Login(_ context.Context, username, password, clientKey string) (application.LoginResult, error) {
	f.lastLogin = loginCall{username: username, password: password, clientKey: clientKey}
	if f.loginErr != nil {
		return application.LoginResult{}, f.loginErr
	}
	return f.loginResult, nil
}

func (f *fakeAuthService) Authenticate(_ context.Context, token string) (application.AuthSession, error) {
	if token == testSessionValue {
		return f.session, nil
	}
	return application.AuthSession{}, application.ErrSessionInvalid
}

func (f *fakeAuthService) Logout(_ context.Context, token string) error {
	f.loggedOut = append(f.loggedOut, token)
	return f.logoutErr
}

func (f *fakeAuthService) ChangePassword(_ context.Context, session application.AuthSession, current, next string) error {
	f.lastChange = changePasswordCall{current: current, next: next}
	f.lastChangeFor = session
	return f.changeErr
}

func withTestSession(req *http.Request) {
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testSessionValue})
}

func withBearer(token string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func authTestConfig() *config.Config {
	return &config.Config{
		GatewayInternalAuthToken: "gateway-token",
		SessionCookieSecure:      "auto",
		RegistryActorTokens: map[domain.Actor]string{
			domain.ActorScheduler:    "scheduler-token",
			domain.ActorOrchestrator: "orchestrator-token",
			domain.ActorPolicy:       "policy-token",
		},
	}
}

func authTestServer(auth AuthService) http.Handler {
	srv := NewServer(authTestConfig(), discardLogger(), nil, nil)
	if auth != nil {
		srv.WithAuth(auth)
	}
	srv.WithOperator(&fakeOperatorService{})
	return srv.Routes()
}

func doAuth(t *testing.T, handler http.Handler, method, path, body string, session bool, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if session {
		withTestSession(req)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestLoginSetsHardenedSessionCookie(t *testing.T) {
	auth := newFakeAuthService()
	handler := authTestServer(auth)
	rec := doAuth(t, handler, http.MethodPost, "/api/v1/auth/login",
		`{"username":"alice","password":"correct horse battery"}`, false,
		map[string]string{"X-Forwarded-Proto": "https"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if auth.lastLogin.username != "alice" || auth.lastLogin.password != "correct horse battery" {
		t.Fatalf("login call = %+v", auth.lastLogin)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookieName || cookie.Value != "minted-session-token" {
		t.Fatalf("cookie = %+v", cookie)
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("cookie attributes = %+v", cookie)
	}
	var body authSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Username != "alice" || body.ExpiresAt == "" {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "minted-session-token") {
		t.Fatal("the session token must never appear in the response body")
	}
}

func TestLoginCookieIsNotSecureOverPlainHTTP(t *testing.T) {
	handler := authTestServer(newFakeAuthService())
	rec := doAuth(t, handler, http.MethodPost, "/api/v1/auth/login",
		`{"username":"alice","password":"correct horse battery"}`, false, nil)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Secure {
		t.Fatalf("plain HTTP cookie must not be Secure: %+v", cookies)
	}
}

func TestLoginFailurePaths(t *testing.T) {
	auth := newFakeAuthService()
	auth.loginErr = application.ErrInvalidCredentials
	rec := doAuth(t, authTestServer(auth), http.MethodPost, "/api/v1/auth/login",
		`{"username":"alice","password":"wrong"}`, false, nil)
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("invalid credentials: status=%d cookies=%v", rec.Code, rec.Result().Cookies())
	}
	if !strings.Contains(rec.Body.String(), "invalid_credentials") {
		t.Fatalf("body = %s", rec.Body)
	}

	auth.loginErr = application.ErrTooManyAttempts
	rec = doAuth(t, authTestServer(auth), http.MethodPost, "/api/v1/auth/login",
		`{"username":"alice","password":"wrong"}`, false, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "900" {
		t.Fatalf("throttled: status=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestMeRequiresASession(t *testing.T) {
	handler := authTestServer(newFakeAuthService())
	if rec := doAuth(t, handler, http.MethodGet, "/api/v1/auth/me", "", false, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous me = %d", rec.Code)
	}
	rec := doAuth(t, handler, http.MethodGet, "/api/v1/auth/me", "", true, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"username":"alice"`) {
		t.Fatalf("me = %d %s", rec.Code, rec.Body)
	}
}

func TestAuthenticatedRequestsRefreshOnlyRenewedCookies(t *testing.T) {
	for _, path := range []string{"/api/v1/auth/me", "/api/v1/operator/overview", "/api/v1/events"} {
		for _, mode := range []string{"auto", "always", "never"} {
			for _, renewed := range []bool{false, true} {
				t.Run(path+"/"+mode+"/"+fmt.Sprint(renewed), func(t *testing.T) {
					auth := newFakeAuthService()
					auth.session.ExpiresAt = time.Now().UTC().Add(application.SessionTTL).Truncate(time.Second)
					auth.session.Renewed = renewed
					cfg := authTestConfig()
					cfg.SessionCookieSecure = mode
					srv := NewServer(cfg, discardLogger(), nil, nil).WithAuth(auth).WithOperator(&fakeOperatorService{})
					req := httptest.NewRequest(http.MethodGet, path, nil)
					withTestSession(req)
					if mode == "auto" {
						req.Header.Set("X-Forwarded-Proto", "https")
					}
					// Stop SSE after authentication rather than waiting for a stream.
					ctx, cancel := context.WithCancel(req.Context())
					cancel()
					rec := httptest.NewRecorder()
					srv.Routes().ServeHTTP(rec, req.WithContext(ctx))
					cookies := rec.Result().Cookies()
					if !renewed {
						if len(cookies) != 0 {
							t.Fatalf("unchanged session reset cookie: %+v", cookies)
						}
						return
					}
					if len(cookies) != 1 {
						t.Fatalf("renewed cookies = %+v", cookies)
					}
					cookie := cookies[0]
					if cookie.Value != testSessionValue || !cookie.Expires.Equal(auth.session.ExpiresAt) || cookie.MaxAge <= 0 ||
						!cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure != (mode != "never") {
						t.Fatalf("renewed cookie = %+v", cookie)
					}
				})
			}
		}
	}
}

func TestLoginUsesForwardedClientInsteadOfSharedProxyAddress(t *testing.T) {
	auth := newFakeAuthService()
	handler := authTestServer(auth)
	for _, client := range []string{"192.0.2.1", "192.0.2.2"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"operator","password":"wrong"}`))
		req.RemoteAddr = "10.0.0.2:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", client)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login status = %d: %s", rec.Code, rec.Body)
		}
		if auth.lastLogin.clientKey != client {
			t.Fatalf("client key = %q, want %q", auth.lastLogin.clientKey, client)
		}
	}
}

func TestLogoutRevokesAndClearsTheCookie(t *testing.T) {
	auth := newFakeAuthService()
	rec := doAuth(t, authTestServer(auth), http.MethodPost, "/api/v1/auth/logout", "", true, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(auth.loggedOut) != 1 || auth.loggedOut[0] != testSessionValue {
		t.Fatalf("logged out = %v", auth.loggedOut)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 || cookies[0].Value != "" {
		t.Fatalf("clearing cookie = %+v", cookies)
	}
}

func TestPasswordChangeRequiresSessionAndMapsPolicyErrors(t *testing.T) {
	handler := authTestServer(newFakeAuthService())
	body := `{"current_password":"old password here","new_password":"new password here"}`
	if rec := doAuth(t, handler, http.MethodPost, "/api/v1/auth/password", body, false, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous password change = %d", rec.Code)
	}

	auth := newFakeAuthService()
	auth.changeErr = application.ErrWeakPassword
	rec := doAuth(t, authTestServer(auth), http.MethodPost, "/api/v1/auth/password", body, true, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "weak_password") {
		t.Fatalf("weak password = %d %s", rec.Code, rec.Body)
	}

	auth.changeErr = application.ErrInvalidCredentials
	rec = doAuth(t, authTestServer(auth), http.MethodPost, "/api/v1/auth/password", body, true, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current = %d", rec.Code)
	}

	auth = newFakeAuthService()
	rec = doAuth(t, authTestServer(auth), http.MethodPost, "/api/v1/auth/password", body, true, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("success = %d %s", rec.Code, rec.Body)
	}
	if auth.lastChange.current != "old password here" || auth.lastChange.next != "new password here" {
		t.Fatalf("change call = %+v", auth.lastChange)
	}
	if auth.lastChangeFor.Username != "alice" {
		t.Fatalf("session identity = %+v", auth.lastChangeFor)
	}
}
