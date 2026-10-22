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

-- name: RevokeApiClient :execrows
UPDATE identity.api_clients SET revoked_at = now()
WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL;
