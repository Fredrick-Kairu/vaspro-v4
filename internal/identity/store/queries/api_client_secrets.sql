-- name: CreateAPIClientSecret :one
INSERT INTO identity.api_client_secrets (id, api_client_id, secret_hash, last4, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListActiveSecretsByClient :many
SELECT * FROM identity.api_client_secrets
WHERE api_client_id = $1 AND revoked_at IS NULL;
