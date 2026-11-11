-- name: CreateUsageBatch :one
INSERT INTO ledger.usage_batches
  (id, tenant_id, account_id, window_start, window_end, events, tokens, transaction_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetUsageBatch :one
SELECT * FROM ledger.usage_batches WHERE id = $1;

-- name: CreateUsageBatchIfNew :one
-- One batch per wallet per window: a rerun of the window, or another
-- replica's run, finds the batch already there and gets no row.
INSERT INTO ledger.usage_batches
  (id, tenant_id, account_id, window_start, window_end, events, tokens)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (account_id, window_start) DO NOTHING
RETURNING *;

-- name: SetUsageBatchTransaction :exec
UPDATE ledger.usage_batches SET transaction_id = $2 WHERE id = $1;
