-- name: CreateSSOConnection :one
INSERT INTO identity.sso_connections (id, tenant_id, protocol, domain, config)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetSSOConnectionByDomain :one
SELECT * FROM identity.sso_connections WHERE domain = $1;
