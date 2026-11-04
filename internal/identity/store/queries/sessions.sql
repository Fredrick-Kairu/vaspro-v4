-- name: CreateSession :one
INSERT INTO identity.sessions (id, user_id, tenant_id, ip, user_agent, expires_at, token_hash, auth_method)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetSessionByID :one
SELECT * FROM identity.sessions WHERE id = $1;

-- name: GetActiveSessionByID :one
SELECT * FROM identity.sessions
WHERE id = $1 AND revoked_at IS NULL AND expires_at > @now::timestamptz;

-- name: GetActiveSessionByTokenHash :one
-- The per-request bearer check: the bearer is the session token, never
-- the session id (migration 20261025090000).
SELECT * FROM identity.sessions
WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > @now::timestamptz;

-- name: ListActiveSessionsByUserID :many
SELECT * FROM identity.sessions
WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > @now::timestamptz
ORDER BY last_seen_at DESC;

-- name: RevokeSession :one
-- Returns token_hash so the caller can evict the session's Redis
-- cache entry, which is keyed by it. No row means not found.
UPDATE identity.sessions SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
RETURNING token_hash;

-- name: RevokeAllSessions :many
UPDATE identity.sessions SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL
RETURNING token_hash;

-- name: UpdateSessionLastSeenAt :exec
UPDATE identity.sessions SET last_seen_at = now() WHERE id = $1;

-- name: SetSessionStepUp :exec
UPDATE identity.sessions SET step_up_at = now() WHERE id = $1;

-- name: SetSessionTenant :execrows
-- Selects the account an active session works in (task 2.2c); only the
-- session's own user can, and the caller has already asked accounts.
UPDATE identity.sessions SET tenant_id = @tenant_id
WHERE id = @id AND user_id = @user_id AND revoked_at IS NULL AND expires_at > @now::timestamptz;
