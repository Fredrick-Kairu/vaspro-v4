-- name: CreateSigningKey :one
INSERT INTO identity.signing_keys (kid, algorithm, public_key, private_ref, status)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListActiveSigningKeys :many
SELECT * FROM identity.signing_keys WHERE status = 'active' ORDER BY created_at;
