-- name: CreateUsageBatch :one
INSERT INTO ledger.usage_batches
  (id, tenant_id, account_id, window_start, window_end, events, tokens, transaction_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetUsageBatch :one
SELECT * FROM ledger.usage_batches WHERE id = $1;
