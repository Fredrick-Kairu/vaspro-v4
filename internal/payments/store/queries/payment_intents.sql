-- name: CreatePaymentIntent :one
INSERT INTO payments.payment_intents (id, tenant_id, purpose, rail_account_id, method, amount_minor, currency,
  payer_phone_e164, account_reference, invoice_id, payment_link_id, idempotency_key, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- name: GetPaymentIntent :one
SELECT * FROM payments.payment_intents WHERE id = $1;

-- GetPaymentIntentByRailRequest finds the intent a callback is for. A
-- callback has no tenant, so this runs as payments_system.
-- name: GetPaymentIntentByRailRequest :one
SELECT * FROM payments.payment_intents WHERE rail_account_id = $1 AND rail_request_id = $2;

-- name: GetPaymentIntentByIdempotencyKey :one
SELECT * FROM payments.payment_intents WHERE tenant_id = $1 AND idempotency_key = $2;

-- name: GetPaymentIntentForUpdate :one
SELECT * FROM payments.payment_intents WHERE id = $1 FOR UPDATE;

-- MarkPaymentIntentPending records the rail's request ID once the push
-- is accepted; only a created intent moves.
-- name: MarkPaymentIntentPending :execrows
UPDATE payments.payment_intents SET status = 'pending', rail_request_id = $2
WHERE id = $1 AND status = 'created';

-- name: CompletePaymentIntent :one
UPDATE payments.payment_intents
SET status = $2, rail_receipt = $3, result_code = $4, error_code = $5, completed_at = $6
WHERE id = $1
RETURNING *;

-- SetPaymentIntentResultCode records a rail result that doesn't end the
-- intent: a query reporting a payment with no confirmation (OQ-22).
-- name: SetPaymentIntentResultCode :exec
UPDATE payments.payment_intents SET result_code = $2 WHERE id = $1;

-- ListPaymentIntentsToExpire is the expiry sweep's work, across
-- tenants (payments_system): created ones never sent, and pending ones
-- not yet answered by a query.
-- name: ListPaymentIntentsToExpire :many
SELECT * FROM payments.payment_intents
WHERE status IN ('created', 'pending') AND expires_at < $1
  AND (status = 'created' OR result_code IS NULL)
ORDER BY expires_at
LIMIT $2;
