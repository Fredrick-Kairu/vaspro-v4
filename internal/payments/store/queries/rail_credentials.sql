-- name: CreateRailCredential :one
INSERT INTO payments.rail_credentials (id, rail_account_id, tenant_id, kind, ciphertext, kms_key_id, fingerprint, status, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', $8)
RETURNING *;

-- name: GetActiveRailCredential :one
SELECT * FROM payments.rail_credentials WHERE rail_account_id = $1 AND kind = $2 AND status = 'active';

-- name: RetireActiveRailCredential :execrows
UPDATE payments.rail_credentials SET status = 'retired', retired_at = $3
WHERE rail_account_id = $1 AND kind = $2 AND status = 'active';

-- name: ListActiveRailCredentials :many
SELECT * FROM payments.rail_credentials WHERE rail_account_id = $1 AND status = 'active' ORDER BY kind;
