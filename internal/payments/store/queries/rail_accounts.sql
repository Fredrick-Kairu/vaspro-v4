-- name: CreateRailAccount :one
INSERT INTO payments.rail_accounts (id, tenant_id, rail_code, account_type, number, store_number, display_name, uses, status, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetRailAccount :one
SELECT * FROM payments.rail_accounts WHERE id = $1;
