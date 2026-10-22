-- name: CreateSession :one
INSERT INTO identity.sessions (id, user_id, tenant_id, ip, user_agent, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSessionByID :one
SELECT * FROM identity.sessions WHERE id = $1;

-- name: GetActiveSessionByID :one
SELECT * FROM identity.sessions
WHERE id = $1 AND revoked_at IS NULL AND expires_at > @now::timestamptz;

-- name: ListActiveSessionsByUserID :many
SELECT * FROM identity.sessions
WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > @now::timestamptz
ORDER BY last_seen_at DESC;

-- name: RevokeSession :execrows
UPDATE identity.sessions SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: RevokeAllSessions :many
UPDATE identity.sessions SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL
RETURNING id;

-- name: UpdateSessionLastSeenAt :exec
UPDATE identity.sessions SET last_seen_at = now() WHERE id = $1;

-- name: SetSessionStepUp :exec
UPDATE identity.sessions SET step_up_at = now() WHERE id = $1;
