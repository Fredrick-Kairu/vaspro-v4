-- name: CreateEntry :one
-- Insert a posting's entries in account_id order, so two postings never
-- lock the same balance rows in opposite orders (ADR 0010).
INSERT INTO ledger.entries
  (transaction_id, account_id, direction, amount_minor, tokens, tax_code, currency)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListEntriesByTransaction :many
SELECT * FROM ledger.entries WHERE transaction_id = $1 ORDER BY id;
