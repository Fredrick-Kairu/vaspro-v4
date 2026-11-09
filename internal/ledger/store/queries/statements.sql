-- name: StatementPage :many
-- One page of an account's statement, newest first (task 3.9, OQ-20),
-- each line with the balance and tokens after it. One statement, so
-- one snapshot: the balance row, the lots and the entries agree
-- (balance sync, ADR 0010). The balance after a line is the balance
-- now, less every line newer than the page's newest, less the lines
-- above it on the page; the cost is the lines newer than the page, not
-- the whole history. Tokens run the same way from the lots' remaining.
WITH acct AS (
  SELECT a.id, ledger.normal_side(a.type) AS side FROM ledger.accounts a WHERE a.id = @account_id
),
page AS (
  SELECT e.id, e.transaction_id, e.direction, e.amount_minor, e.tokens,
         t.posted_at, t.type, t.description, t.reference_type, t.reference_id,
         CASE WHEN e.direction = acct.side THEN e.amount_minor ELSE -e.amount_minor END AS delta,
         CASE WHEN e.direction = acct.side THEN coalesce(e.tokens, 0) ELSE -coalesce(e.tokens, 0) END AS token_delta
  FROM ledger.entries e
  JOIN ledger.transactions t ON t.id = e.transaction_id
  CROSS JOIN acct
  WHERE e.account_id = @account_id
    AND (sqlc.narg(before)::bigint IS NULL OR e.id < sqlc.narg(before))
    AND (sqlc.narg(from_at)::timestamptz IS NULL OR t.posted_at >= sqlc.narg(from_at))
    AND (sqlc.narg(to_at)::timestamptz IS NULL OR t.posted_at < sqlc.narg(to_at))
  ORDER BY e.id DESC
  LIMIT sqlc.arg(max_rows)
),
newer AS (
  SELECT coalesce(sum(CASE WHEN e.direction = acct.side THEN e.amount_minor ELSE -e.amount_minor END), 0) AS minor,
         coalesce(sum(CASE WHEN e.direction = acct.side THEN coalesce(e.tokens, 0) ELSE -coalesce(e.tokens, 0) END), 0) AS tokens
  FROM ledger.entries e CROSS JOIN acct
  WHERE e.account_id = @account_id AND e.id > (SELECT coalesce(max(id), 9223372036854775807) FROM page)
),
now_ AS (
  SELECT (SELECT b.balance_minor FROM ledger.balances b WHERE b.account_id = @account_id) AS minor,
         (SELECT coalesce(sum(l.tokens_remaining), 0) FROM ledger.token_lots l WHERE l.account_id = @account_id) AS tokens
)
SELECT p.id AS entry_id, p.transaction_id, p.direction, p.amount_minor, coalesce(p.tokens::text, '')::text AS tokens,
       p.posted_at, p.type, p.description, p.reference_type, p.reference_id,
       (now_.minor - newer.minor
        - coalesce(sum(p.delta) OVER (ORDER BY p.id DESC ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING), 0))::bigint AS balance_after,
       (now_.tokens - newer.tokens
        - coalesce(sum(p.token_delta) OVER (ORDER BY p.id DESC ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING), 0))::text AS tokens_after
FROM page p CROSS JOIN newer CROSS JOIN now_
ORDER BY p.id DESC;

-- name: ListTenantPricesInForce :many
-- The active list rows and the tenant's own active rows for every
-- service and currency (task 3.9); domain.PricesInForce picks what
-- applies at a time.
SELECT * FROM ledger.token_prices
WHERE status = 'active' AND (tenant_id IS NULL OR tenant_id = @tenant_id)
ORDER BY service_code, currency, min_tokens;
