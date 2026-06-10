-- name: UpsertUser :one
-- Insert a user keyed by their GitHub user id, updating profile fields on
-- repeat logins. Returns the full row (id is stable across logins).
INSERT INTO users (github_user_id, login, email, avatar_url)
VALUES ($1, $2, $3, $4)
ON CONFLICT (github_user_id) DO UPDATE
    SET login = EXCLUDED.login,
        email = EXCLUDED.email,
        avatar_url = EXCLUDED.avatar_url
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetSessionByTokenHash :one
-- Look up a live (unexpired, unrevoked) session by its token hash, returning
-- the session together with the owning user.
SELECT
    sqlc.embed(sessions),
    sqlc.embed(users)
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = $1
  AND sessions.revoked_at IS NULL
  AND sessions.expires_at > now();

-- name: TouchSession :exec
-- Slide the expiry forward on use (sliding-refresh sessions).
UPDATE sessions SET expires_at = $2 WHERE id = $1;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = now()
WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: UpsertGitHubLink :one
INSERT INTO github_links (user_id, metadata, secret_ref, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (user_id) DO UPDATE
    SET metadata = EXCLUDED.metadata,
        secret_ref = EXCLUDED.secret_ref,
        updated_at = now()
RETURNING *;

-- name: GetGitHubLink :one
SELECT * FROM github_links WHERE user_id = $1;
