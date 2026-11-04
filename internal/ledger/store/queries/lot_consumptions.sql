-- name: CreateLotConsumption :one
INSERT INTO ledger.lot_consumptions (lot_id, transaction_id, tokens, value_minor)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListLotConsumptionsByTransaction :many
SELECT * FROM ledger.lot_consumptions WHERE transaction_id = $1 ORDER BY lot_id;
