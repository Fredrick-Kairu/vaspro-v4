-- name: CreateTenant :one
INSERT INTO accounts.tenants
  (id, type, parent_tenant_id, legal_name, display_name, country_code, timezone, default_currency, region, billing_email)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetTenantByID :one
SELECT * FROM accounts.tenants WHERE id = $1;

-- name: GetTenantForUpdate :one
-- Locks the row so two tier changes for one tenant can't interleave.
SELECT * FROM accounts.tenants WHERE id = $1 FOR UPDATE;

-- name: UpdateTenantTier :exec
-- Only valid alongside CreateTierChange in the same transaction: the
-- tenants_tier_change_recorded trigger refuses it otherwise.
UPDATE accounts.tenants
SET tier = $2, version = version + 1, updated_at = now()
WHERE id = $1;

-- name: UpdateTenantType :exec
UPDATE accounts.tenants
SET type = $2, version = version + 1, updated_at = now()
WHERE id = $1;

-- name: ListSubAccounts :many
-- ids are UUIDv7, so this is oldest first.
SELECT * FROM accounts.tenants WHERE parent_tenant_id = $1 ORDER BY id;
