-- name: CreateApiClient :one
INSERT INTO identity.api_clients (
  id, tenant_id, name, environment, scopes, ip_allowlist, expires_at,
  created_by, spend_cap_minor, spend_cap_currency
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetApiClientByID :one
-- Ownership-checked in the query itself, not just trusted to RLS —
-- same "belt and suspenders" style already used for passkeys/sessions.
SELECT * FROM identity.api_clients WHERE id = $1 AND tenant_id = $2;

-- name: ListApiClientsByTenant :many
SELECT * FROM identity.api_clients WHERE tenant_id = $1 ORDER BY created_at DESC;

-- name: GetApiClientByIDAnyTenant :one
-- "AnyTenant" flags at the call site that this skips the normal
-- tenant-checked read above — only task 1.8's OAuth2 token exchange
-- calls this, inside db.Pool.WithRoleTx(ctx, "identity_token_exchange",
-- ...), relying on that role's own narrow SELECT-only RLS policy
-- (migration 20261022223744) instead of an ownership check, since the
-- caller has no session/tenant context to check against yet.
SELECT * FROM identity.api_clients WHERE id = $1;

-- name: RevokeApiClient :execrows
UPDATE identity.api_clients SET revoked_at = now()
WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL;
