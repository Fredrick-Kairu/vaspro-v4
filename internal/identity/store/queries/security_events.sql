-- name: CreateSecurityEvent :one
INSERT INTO identity.security_events (id, user_id, tenant_id, type, ip, detail)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListSecurityEventsByUser :many
SELECT * FROM identity.security_events WHERE user_id = $1 ORDER BY occurred_at DESC;
