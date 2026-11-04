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
SET status = $2, captured_minor = $3, captured_tokens = $4, captured_events = $5, settled_at = $6
WHERE id = $1;

-- name: ListHoldsByAccount :many
SELECT * FROM ledger.holds WHERE account_id = $1 ORDER BY created_at, id;

-- name: ListSettleableWallets :many
-- Settlement's work list across clients (task 3.5), run as
-- ledger_settlement: every wallet with captured, unsettled token holds.
SELECT DISTINCT tenant_id, account_id FROM ledger.holds
WHERE status = 'captured' AND settled_at IS NULL AND tokens IS NOT NULL
ORDER BY tenant_id, account_id;

-- name: LockCapturedHolds :many
-- A wallet's captured, unsettled token holds, locked by id: a capture
-- locks its hold before its balance, and so does settlement.
SELECT * FROM ledger.holds
WHERE account_id = $1 AND status = 'captured' AND settled_at IS NULL AND tokens IS NOT NULL
ORDER BY id
FOR UPDATE;

-- name: SettleHolds :exec
UPDATE ledger.holds SET settled_at = @settled_at, usage_batch_id = @usage_batch_id WHERE id = ANY(@ids::uuid[]);
