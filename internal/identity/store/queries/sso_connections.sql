-- name: CreateSSOConnection :one
INSERT INTO identity.sso_connections (id, tenant_id, protocol, domain, config)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetSSOConnectionByDomain :one
SELECT * FROM identity.sso_connections WHERE domain = $1;

-- name: GetEnabledSSOConnectionByDomain :one
-- Cross-tenant by design: runs only inside WithRoleTx as
-- identity_sso_domain_lookup (migration 20261030120000).
SELECT * FROM identity.sso_connections WHERE domain = $1 AND enabled;
