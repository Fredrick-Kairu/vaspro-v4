-- name: CreateTokenPrice :one
INSERT INTO ledger.token_prices
  (id, tenant_id, service_code, currency, min_tokens, minor_per_token, deal_reference, status, effective_from, effective_to, created_by, approved_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetTokenPrice :one
SELECT * FROM ledger.token_prices WHERE id = $1;

-- name: ListActivePrices :many
-- The active list rows and the tenant's own active rows for a service
-- and currency; domain.SelectPrice picks the one in force.
SELECT * FROM ledger.token_prices
WHERE service_code = $1 AND currency = $2 AND status = 'active'
  AND (tenant_id IS NULL OR tenant_id = $3);
