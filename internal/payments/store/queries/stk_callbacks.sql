-- InsertSTKCallback returns no row (pgx.ErrNoRows) for a checkout
-- already recorded: a repeated callback is ignored. payments_system only.
-- name: InsertSTKCallback :one
INSERT INTO payments.stk_callbacks (id, rail_account_id, checkout_request_id, merchant_request_id, result_code, raw)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (rail_account_id, checkout_request_id) DO NOTHING
RETURNING *;

-- name: GetSTKCallback :one
SELECT * FROM payments.stk_callbacks WHERE rail_account_id = $1 AND checkout_request_id = $2;

-- name: MarkSTKCallbackApplied :execrows
UPDATE payments.stk_callbacks SET payment_intent_id = $2, applied_at = $3
WHERE id = $1 AND applied_at IS NULL;

-- name: ListUnappliedSTKCallbacks :many
SELECT * FROM payments.stk_callbacks WHERE applied_at IS NULL ORDER BY received_at LIMIT $1;
