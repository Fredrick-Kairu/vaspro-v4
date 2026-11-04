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

-- name: ListTokenPricesPage :many
-- Staff's list across clients (task 3.4b), run as ledger_price_review:
-- oldest first (ids are UUIDv7), filtered, after a cursor.
SELECT * FROM ledger.token_prices
WHERE (sqlc.narg(tenant_id)::uuid IS NULL OR tenant_id = sqlc.narg(tenant_id))
  AND (sqlc.narg(service_code)::text IS NULL OR service_code = sqlc.narg(service_code))
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(after)::uuid IS NULL OR id > sqlc.narg(after))
ORDER BY id
LIMIT sqlc.arg(max_rows);

-- name: LockTokenPrice :one
SELECT * FROM ledger.token_prices WHERE id = $1 FOR UPDATE;

-- name: ApproveTokenPrice :one
-- The checker's approval; the CHECKs refuse a self-approval and the
-- EXCLUDE an overlap with an active price.
UPDATE ledger.token_prices SET status = 'active', approved_by = $2 WHERE id = $1 RETURNING *;

-- name: WithdrawTokenPrice :one
UPDATE ledger.token_prices SET status = 'withdrawn' WHERE id = $1 RETURNING *;
