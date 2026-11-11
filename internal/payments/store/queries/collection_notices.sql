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

-- name: GetCollectionNoticeByReceipt :one
SELECT * FROM payments.collection_notices WHERE rail_account_id = $1 AND rail_receipt = $2;

-- ListUnpostedNotices is every top-up on VASPRO's paybill matched to a
-- client but not yet in the ledger (payments_system).
-- name: ListUnpostedNotices :many
SELECT * FROM payments.collection_notices
WHERE tenant_id IS NULL AND match_status = 'matched' AND ledger_transaction_id IS NULL
ORDER BY received_at
LIMIT $1;

-- name: SetNoticeLedgerTransaction :execrows
UPDATE payments.collection_notices SET ledger_transaction_id = $2
WHERE id = $1 AND ledger_transaction_id IS NULL;

-- name: GetCollectionNoticeForUpdate :one
SELECT * FROM payments.collection_notices WHERE id = $1 FOR UPDATE;

-- ListUnpostedSuspense is every payment in suspense on VASPRO's paybill
-- not yet posted into the ledger's suspense account (payments_system).
-- name: ListUnpostedSuspense :many
SELECT * FROM payments.collection_notices
WHERE tenant_id IS NULL AND match_status = 'suspense' AND suspense_transaction_id IS NULL
ORDER BY received_at
LIMIT $1;

-- name: SetNoticeSuspenseTransaction :execrows
UPDATE payments.collection_notices SET suspense_transaction_id = $2
WHERE id = $1 AND suspense_transaction_id IS NULL;

-- MatchCollectionNotice moves a payment out of suspense to a client:
-- only from suspense, so a second match changes nothing.
-- name: MatchCollectionNotice :one
UPDATE payments.collection_notices SET match_status = 'matched', matched_tenant_id = $2
WHERE id = $1 AND match_status = 'suspense'
RETURNING *;

-- ListSuspense is the staff queue: oldest first, after a (paid_at, id)
-- cursor (payments_operations).
-- name: ListSuspense :many
SELECT * FROM payments.collection_notices
WHERE match_status = 'suspense' AND (paid_at, id) > (sqlc.arg(after_paid_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY paid_at, id
LIMIT sqlc.arg(max_rows);

-- ListNoticesPaidBetween is the day's payments to a VASPRO paybill
-- (payments_system), for reconciliation.
-- name: ListNoticesPaidBetween :many
SELECT * FROM payments.collection_notices
WHERE rail_account_id = $1 AND tenant_id IS NULL AND paid_at >= sqlc.arg(paid_from) AND paid_at < sqlc.arg(paid_to)
ORDER BY paid_at, id;

-- name: ListNoticeIDsIn :many
SELECT id FROM payments.collection_notices WHERE id = ANY (sqlc.arg(ids)::uuid[]);
