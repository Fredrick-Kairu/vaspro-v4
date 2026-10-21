-- name: CreateAPIClient :one
INSERT INTO identity.api_clients (id, tenant_id, name, environment, scopes, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAPIClientByID :one
SELECT * FROM identity.api_clients WHERE id = $1;
