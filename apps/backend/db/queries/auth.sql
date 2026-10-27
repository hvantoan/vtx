-- App-hosted operator authentication. Sessions are looked up by the SHA-256 of
-- the cookie value; the raw token never reaches the database.

-- name: UpsertUserPassword :one
INSERT INTO users (username, password_hash)
VALUES (sqlc.arg(username), sqlc.arg(password_hash))
ON CONFLICT (username) DO UPDATE SET
    password_hash = EXCLUDED.password_hash,
    updated_at = CURRENT_TIMESTAMP,
    password_changed_at = CURRENT_TIMESTAMP
RETURNING user_id, username, created_at, password_changed_at;

-- name: GetUserByUsername :one
SELECT user_id, username, password_hash, password_changed_at
FROM users
WHERE username = sqlc.arg(username);

-- name: GetUserByID :one
SELECT user_id, username, password_hash, password_changed_at
FROM users
WHERE user_id = sqlc.arg(user_id);

-- name: UpdateUserPassword :execrows
UPDATE users
SET password_hash = sqlc.arg(password_hash),
    updated_at = CURRENT_TIMESTAMP,
    password_changed_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg(user_id);

-- name: CreateAuthSession :one
INSERT INTO auth_sessions (user_id, token_hash, expires_at)
VALUES (sqlc.arg(user_id), sqlc.arg(token_hash), sqlc.arg(expires_at))
RETURNING session_id, expires_at;

-- name: GetLiveAuthSession :one
SELECT s.session_id, s.user_id, u.username, s.created_at, s.last_seen_at, s.expires_at
FROM auth_sessions s
JOIN users u ON u.user_id = s.user_id
WHERE s.token_hash = sqlc.arg(token_hash)
  AND s.revoked_at IS NULL
  AND s.expires_at > CURRENT_TIMESTAMP;

-- name: TouchAuthSession :execrows
UPDATE auth_sessions
SET last_seen_at = CURRENT_TIMESTAMP,
    expires_at = sqlc.arg(expires_at)
WHERE session_id = sqlc.arg(session_id)
  AND revoked_at IS NULL
  AND expires_at > CURRENT_TIMESTAMP;

-- name: RevokeAuthSession :execrows
UPDATE auth_sessions
SET revoked_at = CURRENT_TIMESTAMP
WHERE session_id = sqlc.arg(session_id)
  AND revoked_at IS NULL;

-- name: RevokeUserSessionsExcept :execrows
UPDATE auth_sessions
SET revoked_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL
  AND session_id <> sqlc.arg(keep_session_id);

-- name: RevokeAllUserSessions :execrows
UPDATE auth_sessions
SET revoked_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- name: PruneAuthSessions :execrows
DELETE FROM auth_sessions
WHERE expires_at <= CURRENT_TIMESTAMP
   OR (revoked_at IS NOT NULL AND revoked_at <= CURRENT_TIMESTAMP - INTERVAL '7 days');
