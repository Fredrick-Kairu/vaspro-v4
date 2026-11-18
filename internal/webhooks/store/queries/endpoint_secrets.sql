-- name: CreateEndpointSecret :one
INSERT INTO webhooks.endpoint_secrets (id, tenant_id, endpoint_id, ciphertext, kms_key_id, fingerprint, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- ListValidEndpointSecrets gives the secrets an attempt is signed with:
-- the current one first, then one still in its rotation overlap.
-- name: ListValidEndpointSecrets :many
SELECT * FROM webhooks.endpoint_secrets
WHERE endpoint_id = $1 AND (expires_at IS NULL OR expires_at > sqlc.arg(at)::timestamptz)
ORDER BY expires_at IS NOT NULL, created_at DESC;

-- ExpireCurrentEndpointSecret starts a rotation: the current secret
-- stays valid until expires_at, so a new one can become current.
-- name: ExpireCurrentEndpointSecret :execrows
UPDATE webhooks.endpoint_secrets SET expires_at = sqlc.arg(expires_at)::timestamptz
WHERE endpoint_id = $1 AND expires_at IS NULL;

-- ExpireOverlappingSecrets ends, at once, a previous secret still in its
-- overlap, so a second rotation leaves no more than two valid.
-- name: ExpireOverlappingSecrets :execrows
UPDATE webhooks.endpoint_secrets SET expires_at = sqlc.arg(at)::timestamptz
WHERE endpoint_id = $1 AND expires_at > sqlc.arg(at)::timestamptz;
