-- name: CreateEntry :one
-- Insert a posting's entries in account_id order, so two postings never
-- lock the same balance rows in opposite orders (ADR 0010).
INSERT INTO ledger.entries
  (transaction_id, account_id, direction, amount_minor, tokens, tax_code, currency)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListEntriesByTransaction :many
SELECT * FROM ledger.entries WHERE transaction_id = $1 ORDER BY id;

-- name: ListPostedLines :many
-- A transaction's lines with their accounts' keys, for answering a
-- replay.
SELECT e.id, e.account_id, e.direction, e.amount_minor, e.currency,
       a.type, a.service_code, a.rail
FROM ledger.entries e
JOIN ledger.accounts a ON a.id = e.account_id
WHERE e.transaction_id = $1
ORDER BY e.id;
