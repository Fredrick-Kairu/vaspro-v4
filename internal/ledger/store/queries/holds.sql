-- name: CreateHold :one
INSERT INTO ledger.holds
  (id, tenant_id, account_id, reference_type, reference_id, amount_minor, tokens, status, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetHoldByReference :one
SELECT * FROM ledger.holds WHERE reference_type = $1 AND reference_id = $2;
