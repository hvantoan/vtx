package application

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

type fakeAuthSession struct {
	AuthSession
	tokenHash string
	revoked   bool
}

type fakeAuthStore struct {
	now      func() time.Time
	users    map[string]AuthUser
	sessions map[string]*fakeAuthSession
	touches  int
	prunes   int
}

func newFakeAuthStore(now func() time.Time) *fakeAuthStore {
	return &fakeAuthStore{now: now, users: map[string]AuthUser{}, sessions: map[string]*fakeAuthSession{}}
}

func (f *fakeAuthStore) UpsertUser(_ context.Context, username, hash string) (AuthUser, error) {
	for id, u := range f.users {
		if u.Username == username {
			u.PasswordHash = hash
			f.users[id] = u
			return u, nil
		}
	}
	u := AuthUser{UserID: fmt.Sprintf("user-%d", len(f.users)+1), Username: username, PasswordHash: hash, PasswordChangedAt: f.now()}
	f.users[u.UserID] = u
	return u, nil
}

func (f *fakeAuthStore) GetUserByUsername(_ context.Context, username string) (AuthUser, error) {
	for _, u := range f.users {
		if u.Username == username {
			return u, nil
		}
	}
	return AuthUser{}, ErrAuthUserNotFound
}

func (f *fakeAuthStore) GetUserByID(_ context.Context, userID string) (AuthUser, error) {
	u, ok := f.users[userID]
	if !ok {
		return AuthUser{}, ErrAuthUserNotFound
	}
	return u, nil
}

func (f *fakeAuthStore) UpdatePassword(_ context.Context, userID, hash string) error {
	u, ok := f.users[userID]
	if !ok {
		return ErrAuthUserNotFound
	}
	u.PasswordHash = hash
	u.PasswordChangedAt = f.now()
	f.users[userID] = u
	return nil
}

func (f *fakeAuthStore) CreateSession(_ context.Context, userID, tokenHash string, expiresAt time.Time) (string, error) {
	id := fmt.Sprintf("session-%d", len(f.sessions)+1)
	user := f.users[userID]
	f.sessions[id] = &fakeAuthSession{
		AuthSession: AuthSession{SessionID: id, UserID: userID, Username: user.Username, CreatedAt: f.now(), LastSeenAt: f.now(), ExpiresAt: expiresAt},
		tokenHash:   tokenHash,
	}
	return id, nil
}

func (f *fakeAuthStore) GetLiveSession(_ context.Context, tokenHash string) (AuthSession, error) {
	for _, s := range f.sessions {
		if s.tokenHash != tokenHash {
			continue
		}
		if s.revoked || !f.now().Before(s.ExpiresAt) {
			return AuthSession{}, ErrAuthSessionNotFound
		}
		return s.AuthSession, nil
	}
	return AuthSession{}, ErrAuthSessionNotFound
}

func (f *fakeAuthStore) TouchSession(_ context.Context, sessionID string, expiresAt time.Time) (bool, error) {
	s, ok := f.sessions[sessionID]
	if !ok || s.revoked || !f.now().Before(s.ExpiresAt) {
		return false, nil
	}
	f.touches++
	s.LastSeenAt = f.now()
	s.ExpiresAt = expiresAt
	return true, nil
}

func (f *fakeAuthStore) RevokeSession(_ context.Context, sessionID string) error {
	if s, ok := f.sessions[sessionID]; ok {
		s.revoked = true
	}
	return nil
}

func (f *fakeAuthStore) RevokeOtherSessions(_ context.Context, userID, keepSessionID string) error {
	for id, s := range f.sessions {
		if s.UserID == userID && id != keepSessionID {
			s.revoked = true
		}
	}
	return nil
}

func (f *fakeAuthStore) RevokeUserSessions(_ context.Context, userID string) error {
	for _, s := range f.sessions {
		if s.UserID == userID {
			s.revoked = true
		}
	}
	return nil
}

func (f *fakeAuthStore) PruneSessions(context.Context) error { f.prunes++; return nil }

const testPassword = "correct horse battery"

func newTestAuthService(t *testing.T) (*AuthService, *fakeAuthStore, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	store := newFakeAuthStore(clock.Now)
	return NewAuthService(store, clock, SessionTTL, nil, nil), store, clock
}

func TestSetPasswordThenLoginStoresOnlyTheHash(t *testing.T) {
	svc, store, _ := newTestAuthService(t)
	if _, err := svc.SetPassword(context.Background(), "Operator", testPassword); err != nil {
		t.Fatalf("set password: %v", err)
	}
	result, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if result.Token == "" || result.Session.Username != "operator" {
		t.Fatalf("login result = %+v", result)
	}
	session := store.sessions[result.Session.SessionID]
	if session == nil {
		t.Fatal("session not stored")
	}
	if session.tokenHash != hashSessionToken(result.Token) || session.tokenHash == result.Token {
		t.Fatal("store must hold the SHA-256 of the token, never the token")
	}
	got, err := svc.Authenticate(context.Background(), result.Token)
	if err != nil || got.Username != "operator" {
		t.Fatalf("authenticate = %+v, %v", got, err)
	}
}

func TestLoginRejectsWrongPasswordThenThrottles(t *testing.T) {
	svc, _, clock := newTestAuthService(t)
	if _, err := svc.SetPassword(context.Background(), "operator", testPassword); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := svc.Login(context.Background(), "operator", "wrong password", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: want ErrInvalidCredentials, got %v", i+1, err)
		}
	}
	if _, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.1"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("want ErrTooManyAttempts, got %v", err)
	}
	// A different client address is throttled independently.
	if _, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.2"); err != nil {
		t.Fatalf("other client should still log in: %v", err)
	}
	clock.now = clock.now.Add(15*time.Minute + time.Second)
	if _, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.1"); err != nil {
		t.Fatalf("after window: %v", err)
	}
}

func TestUnknownUserAndInvalidUsernameFailUniformly(t *testing.T) {
	svc, store, _ := newTestAuthService(t)
	if _, err := svc.Login(context.Background(), "nobody", testPassword, "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	if _, err := svc.Login(context.Background(), "bad name!", testPassword, "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("invalid username: %v", err)
	}
	if len(store.sessions) != 0 {
		t.Fatal("no session may be created for rejected credentials")
	}
}

func TestSessionSlidesAtMostOncePerHour(t *testing.T) {
	svc, store, clock := newTestAuthService(t)
	if _, err := svc.SetPassword(context.Background(), "operator", testPassword); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	clock.now = clock.now.Add(30 * time.Minute)
	got, err := svc.Authenticate(context.Background(), result.Token)
	if err != nil {
		t.Fatal(err)
	}
	if store.touches != 0 || !got.ExpiresAt.Equal(result.ExpiresAt) {
		t.Fatalf("touch within an hour must not write: touches=%d expires=%s", store.touches, got.ExpiresAt)
	}

	clock.now = clock.now.Add(31 * time.Minute)
	got, err = svc.Authenticate(context.Background(), result.Token)
	if err != nil {
		t.Fatal(err)
	}
	if store.touches != 1 {
		t.Fatalf("touches = %d, want 1", store.touches)
	}
	if !got.ExpiresAt.Equal(clock.now.Add(SessionTTL)) {
		t.Fatalf("expires = %s, want %s", got.ExpiresAt, clock.now.Add(SessionTTL))
	}
}

func TestExpiredAndForgedSessionsAreRejected(t *testing.T) {
	svc, _, clock := newTestAuthService(t)
	if _, err := svc.SetPassword(context.Background(), "operator", testPassword); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(SessionTTL)
	if _, err := svc.Authenticate(context.Background(), result.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expired session: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), "forged"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("forged token: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), ""); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("empty token: %v", err)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	svc, _, _ := newTestAuthService(t)
	if _, err := svc.SetPassword(context.Background(), "operator", testPassword); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(context.Background(), result.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(context.Background(), result.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("after logout: %v", err)
	}
	if err := svc.Logout(context.Background(), result.Token); err != nil {
		t.Fatalf("second logout: %v", err)
	}
	if err := svc.Logout(context.Background(), ""); err != nil {
		t.Fatalf("empty logout: %v", err)
	}
}

func TestChangePasswordKeepsCurrentSessionAndRevokesOthers(t *testing.T) {
	svc, _, _ := newTestAuthService(t)
	if _, err := svc.SetPassword(context.Background(), "operator", testPassword); err != nil {
		t.Fatal(err)
	}
	current, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.ChangePassword(context.Background(), current.Session, "not the password", "an even better password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current password: %v", err)
	}
	if err := svc.ChangePassword(context.Background(), current.Session, testPassword, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("short new password: %v", err)
	}

	const next = "an even better password"
	if err := svc.ChangePassword(context.Background(), current.Session, testPassword, next); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), current.Token); err != nil {
		t.Fatalf("current session must survive: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), other.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("other session must be revoked: %v", err)
	}
	if _, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.3"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password must stop working: %v", err)
	}
	if _, err := svc.Login(context.Background(), "operator", next, "10.0.0.3"); err != nil {
		t.Fatalf("new password must work: %v", err)
	}
}

func TestSetPasswordRevokesEverySession(t *testing.T) {
	svc, _, _ := newTestAuthService(t)
	if _, err := svc.SetPassword(context.Background(), "operator", testPassword); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Login(context.Background(), "operator", testPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetPassword(context.Background(), "operator", "a replacement password"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(context.Background(), result.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("reset must revoke live sessions: %v", err)
	}
}

func TestNormalizeUsername(t *testing.T) {
	if got, err := NormalizeUsername("  Ops.Team-1 "); err != nil || got != "ops.team-1" {
		t.Fatalf("normalize = %q, %v", got, err)
	}
	for _, bad := range []string{"", "-lead", "has space", "ünïcode", "x!"} {
		if _, err := NormalizeUsername(bad); !errors.Is(err, ErrUsernameInvalid) {
			t.Fatalf("%q should be invalid, got %v", bad, err)
		}
	}
}
