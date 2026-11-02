-- name: CreateMembership :one
INSERT INTO accounts.memberships (tenant_id, user_id, role_id, status, invited_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetMembership :one
SELECT * FROM accounts.memberships WHERE tenant_id = $1 AND user_id = $2;

-- name: GetActiveMembership :one
SELECT * FROM accounts.memberships
WHERE tenant_id = $1 AND user_id = $2 AND status = 'active';

-- name: ListMembers :many
-- Removed members aren't shown; oldest first.
SELECT * FROM accounts.memberships
WHERE tenant_id = $1 AND status <> 'removed'
ORDER BY created_at, user_id;

-- name: CountActiveMembersWithRole :one
SELECT count(*) FROM accounts.memberships
WHERE tenant_id = $1 AND role_id = $2 AND status = 'active';

-- name: UpdateMembershipRole :exec
UPDATE accounts.memberships SET role_id = $3 WHERE tenant_id = $1 AND user_id = $2;

-- name: RemoveMembership :exec
UPDATE accounts.memberships SET status = 'removed' WHERE tenant_id = $1 AND user_id = $2;

-- name: ListMyMemberships :many
-- Only under the accounts_membership_lookup role (app.MyMemberships),
-- whose policies limit both tables to app.user_id's own memberships.
SELECT m.tenant_id, m.role_id, m.status, m.created_at, t.display_name, t.type, t.tier
FROM accounts.memberships m
JOIN accounts.tenants t ON t.id = m.tenant_id
WHERE m.user_id = $1 AND m.status IN ('active', 'invited')
ORDER BY m.created_at, m.tenant_id;
