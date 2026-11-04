-- name: CreateTokenLot :one
INSERT INTO ledger.token_lots
  (id, tenant_id, account_id, transaction_id, tokens_bought, tokens_remaining, minor_per_token, token_price_id, purchased_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetTokenLot :one
SELECT * FROM ledger.token_lots WHERE id = $1;

-- name: ListOpenLots :many
-- A service wallet's open lots, oldest first: the order FIFO use draws
-- them in. Callers hold the wallet's balance row lock, under which
-- every change to its lots happens.
SELECT * FROM ledger.token_lots
WHERE account_id = $1 AND tokens_remaining > 0
ORDER BY purchased_at, id;

-- name: LockOpenLots :many
SELECT * FROM ledger.token_lots
WHERE account_id = $1 AND tokens_remaining > 0
ORDER BY purchased_at, id
FOR UPDATE;

-- name: DrawFromLot :exec
UPDATE ledger.token_lots SET tokens_remaining = tokens_remaining - @tokens WHERE id = @id;

-- name: GetLotByTransaction :one
SELECT * FROM ledger.token_lots WHERE transaction_id = $1;
