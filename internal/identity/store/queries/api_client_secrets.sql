-- name: CreateApiClientSecret :one
-- api_client_id is trusted here — every caller first resolves it
-- through GetApiClientByID (tenant-checked) before ever calling this.
INSERT INTO identity.api_client_secrets (id, api_client_id, secret_hash, last4)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListActiveApiClientSecretsByClientID :many
-- OQ-01: api_client_secrets has no tenant_id of its own, so every
-- query against it joins through api_clients to scope by tenant —
-- nothing ever queries it standalone.
SELECT s.* FROM identity.api_client_secrets s
JOIN identity.api_clients c ON c.id = s.api_client_id
WHERE s.api_client_id = @api_client_id
  AND c.tenant_id = @tenant_id
  AND s.revoked_at IS NULL
  AND (s.expires_at IS NULL OR s.expires_at > @now::timestamptz)
ORDER BY s.created_at;

-- name: ExpireApiClientSecretForRotation :exec
-- Sets the previous active secret's expires_at to the rotation
-- overlap's end — it keeps working until then, rather than being
-- revoked immediately.
UPDATE identity.api_client_secrets s
SET expires_at = @expires_at::timestamptz
WHERE s.id = @id
  AND EXISTS (
    SELECT 1 FROM identity.api_clients c
    WHERE c.id = s.api_client_id AND c.tenant_id = @tenant_id
  );

-- name: RevokeApiClientSecret :execrows
UPDATE identity.api_client_secrets s
SET revoked_at = now()
WHERE s.id = @id
  AND s.api_client_id = @api_client_id
  AND s.revoked_at IS NULL
  AND EXISTS (
    SELECT 1 FROM identity.api_clients c
    WHERE c.id = s.api_client_id AND c.tenant_id = @tenant_id
  );
