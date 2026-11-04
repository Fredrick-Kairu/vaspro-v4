-- name: CreateAccount :one
-- A client account's balance row is created by trigger (3.2); VASPRO's
-- own accounts have none (ADR 0010).
INSERT INTO ledger.accounts (id, tenant_id, type, service_code, rail, currency)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAccount :one
SELECT * FROM ledger.accounts WHERE id = $1;

-- name: OpenAccount :exec
-- Opens an account by key if it isn't there yet (task 3.3: accounts are
-- opened on first use). Run in the owner's transaction: the tenant's
-- for a client account, ledger_system's for VASPRO's.
INSERT INTO ledger.accounts (id, tenant_id, type, service_code, rail, currency)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, type, service_code, rail, currency) DO NOTHING;

-- name: FindClientAccount :one
SELECT * FROM ledger.accounts
WHERE tenant_id = $1 AND type = $2 AND currency = $3
  AND service_code IS NOT DISTINCT FROM sqlc.narg(service_code)
  AND rail IS NOT DISTINCT FROM sqlc.narg(rail);

-- name: FindSystemAccount :one
SELECT * FROM ledger.accounts
WHERE tenant_id IS NULL AND type = $1 AND currency = $2
  AND service_code IS NOT DISTINCT FROM sqlc.narg(service_code)
  AND rail IS NOT DISTINCT FROM sqlc.narg(rail);

-- name: ListSystemAccountBalances :many
-- VASPRO's accounts have no balance row (ADR 0010): their balance is the
-- sum of their lines on each account's normal side. Run as
-- ledger_system.
SELECT a.id, a.type, a.service_code, a.rail, a.currency, a.status,
       coalesce(sum(CASE WHEN e.direction = ledger.normal_side(a.type) THEN e.amount_minor ELSE -e.amount_minor END), 0)::bigint AS balance_minor
FROM ledger.accounts a
LEFT JOIN ledger.entries e ON e.account_id = a.id
WHERE a.tenant_id IS NULL
GROUP BY a.id
ORDER BY a.type, a.currency, a.service_code, a.rail;
