-- name: CreateTransaction :one
-- A transaction and its entries are inserted in the same database
-- transaction (and savepoint), and must balance per currency by commit
-- (3.2's guards).
INSERT INTO ledger.transactions
  (id, tenant_id, type, idempotency_key, reference_type, reference_id, reverses_id, description, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetTransaction :one
SELECT * FROM ledger.transactions WHERE id = $1;

-- name: GetClientTransactionByKey :one
SELECT * FROM ledger.transactions WHERE tenant_id = $1 AND idempotency_key = $2;

-- name: GetSystemTransactionByKey :one
SELECT * FROM ledger.transactions WHERE tenant_id IS NULL AND idempotency_key = $1;
