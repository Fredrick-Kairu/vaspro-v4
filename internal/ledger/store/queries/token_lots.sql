-- name: CreateTokenLot :one
INSERT INTO ledger.token_lots
  (id, tenant_id, account_id, transaction_id, tokens_bought, tokens_remaining, minor_per_token, token_price_id, purchased_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetTokenLot :one
SELECT * FROM ledger.token_lots WHERE id = $1;
