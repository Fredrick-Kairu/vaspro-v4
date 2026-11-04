-- name: CreateAccount :one
-- A client account's balance row is created by trigger (3.2); VASPRO's
-- own accounts have none (ADR 0010).
INSERT INTO ledger.accounts (id, tenant_id, type, service_code, rail, currency)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAccount :one
SELECT * FROM ledger.accounts WHERE id = $1;
