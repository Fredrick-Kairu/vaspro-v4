-- name: GetBalance :one
-- balance_minor is moved only by the entries trigger (3.2); the service
-- may write held_minor alone.
SELECT * FROM ledger.balances WHERE account_id = $1;
