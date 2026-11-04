-- name: CreateTierChange :one
-- Never set created_at: the tenants_tier_change_recorded trigger relies
-- on its default, now(), to prove the row is from the same transaction
-- as the tier update (migration 20261030140000).
INSERT INTO accounts.tier_changes
  (id, tenant_id, from_tier, to_tier, reason, kyc_check_id, changed_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListTierChangesByTenant :many
SELECT * FROM accounts.tier_changes WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC;
