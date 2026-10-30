-- name: CreateMembership :one
INSERT INTO accounts.memberships (tenant_id, user_id, role_id, status, invited_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetMembership :one
SELECT * FROM accounts.memberships WHERE tenant_id = $1 AND user_id = $2;

-- name: GetActiveMembership :one
SELECT * FROM accounts.memberships
WHERE tenant_id = $1 AND user_id = $2 AND status = 'active';
