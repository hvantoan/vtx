package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// SessionTTL is the sliding lifetime of an operator session.
	SessionTTL = 30 * 24 * time.Hour
	// SessionExtendEvery bounds writes: a busy dashboard slides its cookie
	// expiry at most once per hour instead of on every request.
	SessionExtendEvery = time.Hour
	// MinPasswordChars is the policy minimum for the single operator account.
	MinPasswordChars = 12
	// MaxPasswordBytes caps argon2 input so an oversized body cannot be used
	// to burn CPU deliberately.
	MaxPasswordBytes = 1024
	// sessionTokenBytes is the entropy of the opaque cookie value.
	sessionTokenBytes = 32
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type AuthService struct {
	store         AuthStore
	clock         Clock
	sessionTTL    time.Duration
	extendEvery   time.Duration
	limiter       *LoginLimiter
	logger        *slog.Logger
	dummyHashCost string
}

func NewAuthService(store AuthStore, clock Clock, sessionTTL time.Duration, limiter *LoginLimiter, logger *slog.Logger) *AuthService {
	if clock == nil {
		clock = SystemClock{}
	}
	if sessionTTL <= 0 {
		sessionTTL = SessionTTL
	}
	if limiter == nil {
		limiter = NewLoginLimiter(5, 15*time.Minute, clock)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AuthService{
		store:         store,
		clock:         clock,
		sessionTTL:    sessionTTL,
		extendEvery:   SessionExtendEvery,
		limiter:       limiter,
		logger:        logger,
		dummyHashCost: dummyCredentialHash,
	}
}

// Login verifies credentials and mints a new session. clientKey identifies the
// caller for throttling (the HTTP layer passes the remote address).
func (s *AuthService) Login(ctx context.Context, username, password, clientKey string) (LoginResult, error) {
	normalized, nameErr := NormalizeUsername(username)
	limiterKey := clientKey + "|" + normalized
	if !s.limiter.Allow(limiterKey) {
		return LoginResult{}, ErrTooManyAttempts
	}

	candidateHash := s.dummyHashCost
	var user AuthUser
	found := false
	if nameErr == nil {
		u, err := s.store.GetUserByUsername(ctx, normalized)
		switch {
		case err == nil:
			user, candidateHash, found = u, u.PasswordHash, true
		case !errors.Is(err, ErrAuthUserNotFound):
			return LoginResult{}, fmt.Errorf("lookup user: %w", err)
		}
	}

	ok, err := verifyPassword(candidateHash, password)
	if err != nil {
		return LoginResult{}, fmt.Errorf("verify password: %w", err)
	}
	if !found || !ok {
		s.limiter.Failure(limiterKey)
		return LoginResult{}, ErrInvalidCredentials
	}

	s.limiter.Reset(limiterKey)
	if err := s.store.PruneSessions(ctx); err != nil {
		// Hygiene only; a prune failure must not block a valid login.
		s.logger.Warn("auth session prune failed", slog.Any("error", err))
	}

	token, tokenHash, err := newSessionToken()
	if err != nil {
		return LoginResult{}, err
	}
	now := s.clock.Now()
	expiresAt := now.Add(s.sessionTTL)
	sessionID, err := s.store.CreateSession(ctx, user.UserID, tokenHash, expiresAt)
	if err != nil {
		return LoginResult{}, fmt.Errorf("create session: %w", err)
	}

	return LoginResult{
		Session: AuthSession{
			SessionID:  sessionID,
			UserID:     user.UserID,
			Username:   user.Username,
			CreatedAt:  now,
			LastSeenAt: now,
			ExpiresAt:  expiresAt,
		},
		Token:     token,
		ExpiresAt: expiresAt,
	}, nil
}

// Authenticate resolves a cookie value to a live session, sliding its expiry
// when the session has not been touched for SessionExtendEvery.
func (s *AuthService) Authenticate(ctx context.Context, token string) (AuthSession, error) {
	if token == "" {
		return AuthSession{}, ErrSessionInvalid
	}
	session, err := s.store.GetLiveSession(ctx, hashSessionToken(token))
	if errors.Is(err, ErrAuthSessionNotFound) {
		return AuthSession{}, ErrSessionInvalid
	}
	if err != nil {
		return AuthSession{}, fmt.Errorf("get live session: %w", err)
	}

	now := s.clock.Now()
	if now.Sub(session.LastSeenAt) < s.extendEvery {
		return session, nil
	}
	expiresAt := now.Add(s.sessionTTL)
	touched, err := s.store.TouchSession(ctx, session.SessionID, expiresAt)
	if err != nil {
		s.logger.Warn("auth session slide failed", slog.Any("error", err))
		return session, nil
	}
	if !touched {
		// Revoked or expired between the read and the write: fail closed.
		return AuthSession{}, ErrSessionInvalid
	}
	session.LastSeenAt = now
	session.ExpiresAt = expiresAt
	session.Renewed = true
	return session, nil
}

// Logout revokes the session behind the cookie. Unknown or already-revoked
// values succeed so the endpoint is idempotent.
func (s *AuthService) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	session, err := s.store.GetLiveSession(ctx, hashSessionToken(token))
	if errors.Is(err, ErrAuthSessionNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get live session: %w", err)
	}
	if err := s.store.RevokeSession(ctx, session.SessionID); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// ChangePassword verifies the current password, stores the new hash and
// revokes every other session. The calling session survives.
func (s *AuthService) ChangePassword(ctx context.Context, session AuthSession, currentPassword, newPassword string) error {
	user, err := s.store.GetUserByID(ctx, session.UserID)
	if errors.Is(err, ErrAuthUserNotFound) {
		return ErrSessionInvalid
	}
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	ok, err := verifyPassword(user.PasswordHash, currentPassword)
	if err != nil {
		return fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := s.store.UpdatePassword(ctx, user.UserID, hash); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if err := s.store.RevokeOtherSessions(ctx, user.UserID, session.SessionID); err != nil {
		return fmt.Errorf("revoke other sessions: %w", err)
	}
	return nil
}

// SetPassword is the CLI seed/reset path. It creates or updates the account and
// revokes every existing session, so a reset cannot leave an intruder logged in.
func (s *AuthService) SetPassword(ctx context.Context, username, password string) (AuthUser, error) {
	normalized, err := NormalizeUsername(username)
	if err != nil {
		return AuthUser{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return AuthUser{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return AuthUser{}, fmt.Errorf("hash password: %w", err)
	}
	user, err := s.store.UpsertUser(ctx, normalized, hash)
	if err != nil {
		return AuthUser{}, fmt.Errorf("upsert user: %w", err)
	}
	if err := s.store.RevokeUserSessions(ctx, user.UserID); err != nil {
		return user, fmt.Errorf("revoke user sessions: %w", err)
	}
	return user, nil
}

// NormalizeUsername lower-cases and trims a username and enforces the same
// charset the database CHECK enforces.
func NormalizeUsername(username string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(normalized) {
		return "", ErrUsernameInvalid
	}
	return normalized, nil
}

// ValidatePassword applies the operator password policy. Passwords are never
// trimmed: leading/trailing spaces are legitimate characters.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordChars {
		return fmt.Errorf("%w: use at least %d characters", ErrWeakPassword, MinPasswordChars)
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("%w: use at most %d bytes", ErrWeakPassword, MaxPasswordBytes)
	}
	return nil
}

func newSessionToken() (token, tokenHash string, err error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashSessionToken(token), nil
}

func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
