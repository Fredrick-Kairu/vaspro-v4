-- name: CreatePasskey :one
INSERT INTO identity.user_passkeys (id, user_id, credential_id, public_key, name)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetPasskeyByCredentialID :one
SELECT * FROM identity.user_passkeys WHERE credential_id = $1;
