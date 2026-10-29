-- name: CreatePasskey :one
INSERT INTO identity.user_passkeys (
  id, user_id, credential_id, public_key, name,
  flags, attestation_type, attestation_format, transports
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetPasskeyByCredentialID :one
SELECT * FROM identity.user_passkeys WHERE credential_id = $1;

-- name: GetPasskeyByID :one
SELECT * FROM identity.user_passkeys WHERE id = $1 AND user_id = $2;

-- name: ListPasskeysByUserID :many
SELECT * FROM identity.user_passkeys WHERE user_id = $1 ORDER BY created_at;

-- name: RenamePasskey :one
UPDATE identity.user_passkeys SET name = $3
WHERE id = $1 AND user_id = $2
RETURNING *;

-- name: DeletePasskey :execrows
DELETE FROM identity.user_passkeys WHERE id = $1 AND user_id = $2;

-- name: UpdatePasskeySignCount :exec
UPDATE identity.user_passkeys
SET sign_count = $2, flags = $3, clone_warning = $4, last_used_at = now()
WHERE credential_id = $1;
