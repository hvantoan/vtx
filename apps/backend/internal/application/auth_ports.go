package application

import (
	"context"
	"errors"
	"time"
)

// App-hosted operator authentication. The operator bearer token is gone; a
// session cookie (minted here) is the only interactive credential. Machine
// actor tokens are authenticated outside this package and are untouched.

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrTooManyAttempts    = errors.New("too many login attempts")
	ErrSessionInvalid     = errors.New("session invalid or expired")
	ErrWeakPassword       = errors.New("password does not meet policy")
	ErrUsernameInvalid    = errors.New("username invalid")
)

// Store-level sentinels so the service can distinguish "row missing" from an
// adapter failure.
var (
	ErrAuthUserNotFound    = errors.New("auth store: user not found")
	ErrAuthSessionNotFound = errors.New("auth store: session not found")
)

type AuthUser struct {
	UserID            string
	Username          string
	PasswordHash      string
	PasswordChangedAt time.Time
}

type AuthSession struct {
	SessionID  string
	UserID     string
	Username   string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	// Renewed is response metadata, set only after a successful expiry extension.
	Renewed bool
}

type LoginResult struct {
	Session   AuthSession
	Token     string
	ExpiresAt time.Time
}

type AuthStore interface {
	UpsertUser(ctx context.Context, username, passwordHash string) (AuthUser, error)
	GetUserByUsername(ctx context.Context, username string) (AuthUser, error)
	GetUserByID(ctx context.Context, userID string) (AuthUser, error)
	UpdatePassword(ctx context.Context, userID, passwordHash string) error

	CreateSession(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (string, error)
	GetLiveSession(ctx context.Context, tokenHash string) (AuthSession, error)
	TouchSession(ctx context.Context, sessionID string, expiresAt time.Time) (bool, error)
	RevokeSession(ctx context.Context, sessionID string) error
	RevokeOtherSessions(ctx context.Context, userID, keepSessionID string) error
	RevokeUserSessions(ctx context.Context, userID string) error
	PruneSessions(ctx context.Context) error
}
