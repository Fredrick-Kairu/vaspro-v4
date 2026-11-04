-- name: CreateTokenPrice :one
INSERT INTO ledger.token_prices
  (id, tenant_id, service_code, currency, min_tokens, minor_per_token, deal_reference, status, effective_from, effective_to, created_by, approved_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetTokenPrice :one
SELECT * FROM ledger.token_prices WHERE id = $1;
