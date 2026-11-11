-- InsertCollectionNotice returns no row (pgx.ErrNoRows) when the rail
-- account already has the receipt: a repeated callback is ignored.
-- name: InsertCollectionNotice :one
INSERT INTO payments.collection_notices (id, rail_account_id, tenant_id, rail_receipt, transaction_type,
  amount_minor, currency, bill_ref, payer_phone_e164, payer_name, paid_at, account_balance_minor,
  payment_intent_id, match_status, matched_tenant_id, raw)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (rail_account_id, rail_receipt) DO NOTHING
RETURNING *;

-- name: GetCollectionNotice :one
SELECT * FROM payments.collection_notices WHERE id = $1;
