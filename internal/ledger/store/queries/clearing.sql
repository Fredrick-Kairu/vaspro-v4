-- ListClearingEntries is a rail's provider clearing lines posted in a
-- window, in entry order, for payments' reconciliation (task 4.6). It
-- runs as ledger_audit, which reads every client's postings.
-- name: ListClearingEntries :many
SELECT e.id AS entry_id, e.transaction_id, e.direction::text AS direction, e.amount_minor,
       coalesce(t.reference_type, '')::text AS reference_type, t.reference_id, t.posted_at
FROM ledger.entries e
JOIN ledger.accounts a ON a.id = e.account_id
JOIN ledger.transactions t ON t.id = e.transaction_id
WHERE a.type = 'provider_clearing' AND a.rail = sqlc.arg(rail) AND a.currency = sqlc.arg(currency)
  AND t.posted_at >= sqlc.arg(posted_from) AND t.posted_at < sqlc.arg(posted_to)
  AND e.id > sqlc.arg(after_entry_id)
ORDER BY e.id
LIMIT sqlc.arg(max_rows);
