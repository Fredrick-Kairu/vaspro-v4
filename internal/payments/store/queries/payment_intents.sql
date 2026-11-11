-- name: CreatePaymentIntent :one
INSERT INTO payments.payment_intents (id, tenant_id, purpose, rail_account_id, method, amount_minor, currency,
  payer_phone_e164, account_reference, invoice_id, payment_link_id, idempotency_key, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: GetPaymentIntent :one
SELECT * FROM payments.payment_intents WHERE id = $1;

-- GetPaymentIntentByRailRequest finds the intent a callback is for. A
-- callback has no tenant, so this runs as payments_system.
-- name: GetPaymentIntentByRailRequest :one
SELECT * FROM payments.payment_intents WHERE rail_account_id = $1 AND rail_request_id = $2;
