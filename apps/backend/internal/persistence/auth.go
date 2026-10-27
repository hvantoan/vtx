package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/persistence/db"
)

// PgAuthStore is the pgx adapter for app-hosted operator sessions.
type PgAuthStore struct {
	queries *db.Queries
}

func NewPgAuthStore(queries *db.Queries) *PgAuthStore {
	return &PgAuthStore{queries: queries}
}

func (s *PgAuthStore) UpsertUser(ctx context.Context, username, passwordHash string) (application.AuthUser, error) {
	row, err := s.queries.UpsertUserPassword(ctx, db.UpsertUserPasswordParams{Username: username, PasswordHash: passwordHash})
	if err != nil {
		return application.AuthUser{}, fmt.Errorf("upsert user: %w", err)
	}
	return application.AuthUser{
		UserID:            uuidToString(row.UserID),
		Username:          row.Username,
		PasswordHash:      passwordHash,
		PasswordChangedAt: row.PasswordChangedAt.Time,
	}, nil
}

func (s *PgAuthStore) GetUserByUsername(ctx context.Context, username string) (application.AuthUser, error) {
	row, err := s.queries.GetUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.AuthUser{}, application.ErrAuthUserNotFound
	}
	if err != nil {
		return application.AuthUser{}, fmt.Errorf("get user by username: %w", err)
	}
	return application.AuthUser{
		UserID:            uuidToString(row.UserID),
		Username:          row.Username,
		PasswordHash:      row.PasswordHash,
		PasswordChangedAt: row.PasswordChangedAt.Time,
	}, nil
}

func (s *PgAuthStore) GetUserByID(ctx context.Context, userID string) (application.AuthUser, error) {
	id, err := uuidFromString(userID)
	if err != nil {
		return application.AuthUser{}, application.ErrAuthUserNotFound
	}
	row, err := s.queries.GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.AuthUser{}, application.ErrAuthUserNotFound
	}
	if err != nil {
		return application.AuthUser{}, fmt.Errorf("get user by id: %w", err)
	}
	return application.AuthUser{
		UserID:            uuidToString(row.UserID),
		Username:          row.Username,
		PasswordHash:      row.PasswordHash,
		PasswordChangedAt: row.PasswordChangedAt.Time,
	}, nil
}

func (s *PgAuthStore) UpdatePassword(ctx context.Context, userID, passwordHash string) error {
	id, err := uuidFromString(userID)
	if err != nil {
		return application.ErrAuthUserNotFound
	}
	rows, err := s.queries.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{UserID: id, PasswordHash: passwordHash})
	if err != nil {
		return fmt.Errorf("update user password: %w", err)
	}
	if rows == 0 {
		return application.ErrAuthUserNotFound
	}
	return nil
}

func (s *PgAuthStore) CreateSession(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (string, error) {
	id, err := uuidFromString(userID)
	if err != nil {
		return "", application.ErrAuthUserNotFound
	}
	row, err := s.queries.CreateAuthSession(ctx, db.CreateAuthSessionParams{
		UserID:    id,
		TokenHash: tokenHash,
		ExpiresAt: timestamptzFrom(&expiresAt),
	})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return uuidToString(row.SessionID), nil
}

func (s *PgAuthStore) GetLiveSession(ctx context.Context, tokenHash string) (application.AuthSession, error) {
	row, err := s.queries.GetLiveAuthSession(ctx, tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.AuthSession{}, application.ErrAuthSessionNotFound
	}
	if err != nil {
		return application.AuthSession{}, fmt.Errorf("get live session: %w", err)
	}
	return application.AuthSession{
		SessionID:  uuidToString(row.SessionID),
		UserID:     uuidToString(row.UserID),
		Username:   row.Username,
		CreatedAt:  row.CreatedAt.Time,
		LastSeenAt: row.LastSeenAt.Time,
		ExpiresAt:  row.ExpiresAt.Time,
	}, nil
}

func (s *PgAuthStore) TouchSession(ctx context.Context, sessionID string, expiresAt time.Time) (bool, error) {
	id, err := uuidFromString(sessionID)
	if err != nil {
		return false, nil
	}
	rows, err := s.queries.TouchAuthSession(ctx, db.TouchAuthSessionParams{SessionID: id, ExpiresAt: timestamptzFrom(&expiresAt)})
	if err != nil {
		return false, fmt.Errorf("touch session: %w", err)
	}
	return rows > 0, nil
}

func (s *PgAuthStore) RevokeSession(ctx context.Context, sessionID string) error {
	id, err := uuidFromString(sessionID)
	if err != nil {
		return nil
	}
	if _, err := s.queries.RevokeAuthSession(ctx, id); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func (s *PgAuthStore) RevokeOtherSessions(ctx context.Context, userID, keepSessionID string) error {
	user, err := uuidFromString(userID)
	if err != nil {
		return application.ErrAuthUserNotFound
	}
	keep, err := uuidFromString(keepSessionID)
	if err != nil {
		return fmt.Errorf("parse keep session id: %w", err)
	}
	if _, err := s.queries.RevokeUserSessionsExcept(ctx, db.RevokeUserSessionsExceptParams{UserID: user, KeepSessionID: keep}); err != nil {
		return fmt.Errorf("revoke other sessions: %w", err)
	}
	return nil
}

func (s *PgAuthStore) RevokeUserSessions(ctx context.Context, userID string) error {
	id, err := uuidFromString(userID)
	if err != nil {
		return application.ErrAuthUserNotFound
	}
	if _, err := s.queries.RevokeAllUserSessions(ctx, id); err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

func (s *PgAuthStore) PruneSessions(ctx context.Context) error {
	if _, err := s.queries.PruneAuthSessions(ctx); err != nil {
		return fmt.Errorf("prune sessions: %w", err)
	}
	return nil
}
