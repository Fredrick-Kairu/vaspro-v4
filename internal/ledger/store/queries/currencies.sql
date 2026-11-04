-- name: GetCurrency :one
SELECT * FROM ledger.currencies WHERE code = $1;
