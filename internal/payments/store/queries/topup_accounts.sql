-- name: CreateTopupAccount :one
INSERT INTO payments.topup_accounts (id, tenant_id, rail_account_id, account_number)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetTopupAccountForTenant :one
SELECT * FROM payments.topup_accounts WHERE tenant_id = $1 AND rail_account_id = $2;

-- GetTopupAccountByNumber matches a C2B payment's account number. The
-- payment has no tenant, so this runs as payments_system.
-- name: GetTopupAccountByNumber :one
SELECT * FROM payments.topup_accounts WHERE rail_account_id = $1 AND account_number = $2;

-- InsertTopupAccount returns no row (pgx.ErrNoRows) when the tenant
-- already has a number on the paybill, or the number is taken: the
-- caller reads the tenant's number again and, if it has none, tries a
-- fresh number.
-- name: InsertTopupAccount :one
INSERT INTO payments.topup_accounts (id, tenant_id, rail_account_id, account_number)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING
RETURNING *;
