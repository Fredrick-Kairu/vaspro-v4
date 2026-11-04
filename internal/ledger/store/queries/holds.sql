-- name: CreateHold :one
INSERT INTO ledger.holds
  (id, tenant_id, account_id, reference_type, reference_id, amount_minor, tokens, status, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetHoldByReference :one
SELECT * FROM ledger.holds WHERE reference_type = $1 AND reference_id = $2;

-- name: LockHoldByReference :one
SELECT * FROM ledger.holds WHERE reference_type = $1 AND reference_id = $2 FOR UPDATE;

-- name: UpdateHoldState :exec
UPDATE ledger.holds
SET status = $2, captured_minor = $3, captured_tokens = $4, settled_at = $5
WHERE id = $1;

-- name: ListHoldsByAccount :many
SELECT * FROM ledger.holds WHERE account_id = $1 ORDER BY created_at, id;
