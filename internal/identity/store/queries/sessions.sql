-- name: CreateSession :one
INSERT INTO identity.sessions (id, user_id, tenant_id, ip, user_agent, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSessionByID :one
SELECT * FROM identity.sessions WHERE id = $1;
