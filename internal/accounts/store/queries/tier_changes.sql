-- name: CreateTierChange :one
INSERT INTO accounts.tier_changes
  (id, tenant_id, from_tier, to_tier, reason, kyc_check_id, changed_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListTierChangesByTenant :many
SELECT * FROM accounts.tier_changes WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC;
