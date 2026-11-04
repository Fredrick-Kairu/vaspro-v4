-- name: GetBalance :one
-- balance_minor is moved only by the entries trigger (3.2); the service
-- may write held_minor alone.
SELECT * FROM ledger.balances WHERE account_id = $1;

-- name: LockBalances :many
-- Locks the given client accounts' balance rows in account_id order, so
-- two postings never lock the same rows in opposite orders (ADR 0010).
SELECT b.account_id, b.balance_minor, b.held_minor, a.type, a.status
FROM ledger.balances b
JOIN ledger.accounts a ON a.id = b.account_id
WHERE b.account_id = ANY(@account_ids::uuid[])
ORDER BY b.account_id
FOR UPDATE OF b;

-- name: AdjustHeld :exec
-- held_minor is the only balance column the service may write; the
-- wallet guard (3.2) checks it at commit.
UPDATE ledger.balances
SET held_minor = held_minor + @delta, version = version + 1, updated_at = now()
WHERE account_id = @account_id;

-- name: ListClientBalances :many
-- The tenant's accounts and balances (RLS scopes both to the tenant).
SELECT a.id, a.type, a.service_code, a.rail, a.currency, a.status, b.balance_minor, b.held_minor
FROM ledger.accounts a
JOIN ledger.balances b ON b.account_id = a.id
WHERE a.tenant_id = $1
ORDER BY a.type, a.currency, a.service_code;
