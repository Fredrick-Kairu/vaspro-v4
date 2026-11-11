-- name: CreateRailAccount :one
INSERT INTO payments.rail_accounts (id, tenant_id, rail_code, account_type, number, store_number, display_name, uses, status, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetRailAccount :one
SELECT * FROM payments.rail_accounts WHERE id = $1;

-- GetActiveTopupRailAccount is VASPRO's paybill clients top up on, for
-- a rail. Only an active (verified) one is ever offered.
-- name: GetActiveTopupRailAccount :one
SELECT * FROM payments.rail_accounts
WHERE tenant_id IS NULL AND rail_code = $1 AND status = 'active' AND 'vaspro_topups' = ANY (uses)
ORDER BY verified_at, id
LIMIT 1;

-- name: GetRailAccountForUpdate :one
SELECT * FROM payments.rail_accounts WHERE id = $1 FOR UPDATE;

-- name: SetRailAccountVerified :one
UPDATE payments.rail_accounts SET status = 'active', verified_at = $2, version = version + 1
WHERE id = $1
RETURNING *;

-- SetRailAccountPending puts an account back to pending verification
-- after its credentials change, until a test call works again.
-- name: SetRailAccountPending :execrows
UPDATE payments.rail_accounts SET status = 'pending_verification', verified_at = NULL, version = version + 1
WHERE id = $1 AND status = 'active';

-- name: SetRailAccountC2BRegistered :execrows
UPDATE payments.rail_accounts SET c2b_registered_at = $2, version = version + 1 WHERE id = $1;
