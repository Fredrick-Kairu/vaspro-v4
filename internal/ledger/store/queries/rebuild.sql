-- The nightly rebuild (task 3.8, OQ-19). Each check is one statement,
-- so it reads one snapshot while postings go on; all of them run as
-- ledger_audit, which reads every client's rows.

-- name: RebuildUnbalancedTransactions :many
SELECT e.transaction_id, t.tenant_id, e.currency,
       sum(CASE WHEN e.direction = 'D' THEN e.amount_minor ELSE 0 END)::bigint AS debits,
       sum(CASE WHEN e.direction = 'C' THEN e.amount_minor ELSE 0 END)::bigint AS credits
FROM ledger.entries e
JOIN ledger.transactions t ON t.id = e.transaction_id
GROUP BY e.transaction_id, t.tenant_id, e.currency
HAVING sum(CASE WHEN e.direction = 'D' THEN e.amount_minor ELSE -e.amount_minor END) <> 0
ORDER BY e.transaction_id, e.currency;

-- name: RebuildThinTransactions :many
-- A transaction needs a debit and a credit: fewer than two lines.
SELECT t.id AS transaction_id, t.tenant_id, count(e.id)::bigint AS lines
FROM ledger.transactions t
LEFT JOIN ledger.entries e ON e.transaction_id = t.id
GROUP BY t.id, t.tenant_id
HAVING count(e.id) < 2
ORDER BY t.id;

-- name: RebuildUnbalancedCurrencies :many
SELECT currency,
       sum(CASE WHEN direction = 'D' THEN amount_minor ELSE 0 END)::bigint AS debits,
       sum(CASE WHEN direction = 'C' THEN amount_minor ELSE 0 END)::bigint AS credits
FROM ledger.entries
GROUP BY currency
HAVING sum(CASE WHEN direction = 'D' THEN amount_minor ELSE -amount_minor END) <> 0
ORDER BY currency;

-- name: RebuildBalanceMismatches :many
-- A client account's balance against the replay of its entries on its
-- normal side (ADR 0010). VASPRO's accounts have no balance row.
WITH net AS (
  SELECT account_id, sum(CASE WHEN direction = 'D' THEN amount_minor ELSE -amount_minor END) AS debit_net
  FROM ledger.entries GROUP BY account_id
)
SELECT a.id AS account_id, a.tenant_id, a.currency, b.balance_minor AS actual,
       (CASE WHEN ledger.normal_side(a.type) = 'D' THEN coalesce(n.debit_net, 0) ELSE -coalesce(n.debit_net, 0) END)::bigint AS expected
FROM ledger.accounts a
JOIN ledger.balances b ON b.account_id = a.id
LEFT JOIN net n ON n.account_id = a.id
WHERE b.balance_minor <> (CASE WHEN ledger.normal_side(a.type) = 'D' THEN coalesce(n.debit_net, 0) ELSE -coalesce(n.debit_net, 0) END)
ORDER BY a.id;

-- name: RebuildMissingBalanceRows :many
SELECT a.id AS account_id, a.tenant_id, a.currency
FROM ledger.accounts a
WHERE a.tenant_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM ledger.balances b WHERE b.account_id = a.id)
ORDER BY a.id;

-- name: RebuildHeldMinorMismatches :many
-- Money holds: an active hold holds its amount, a captured one not yet
-- settled its captured amount.
WITH held AS (
  SELECT account_id, sum(CASE
           WHEN status = 'active' THEN amount_minor
           WHEN status = 'captured' AND settled_at IS NULL THEN captured_minor
           ELSE 0 END) AS minor
  FROM ledger.holds WHERE tokens IS NULL GROUP BY account_id
)
SELECT b.account_id, a.tenant_id, a.currency, b.held_minor AS actual, coalesce(h.minor, 0)::bigint AS expected
FROM ledger.balances b
JOIN ledger.accounts a ON a.id = b.account_id
LEFT JOIN held h ON h.account_id = b.account_id
WHERE b.held_minor <> coalesce(h.minor, 0)
ORDER BY b.account_id;

-- name: RebuildHeldTokensMismatches :many
WITH held AS (
  SELECT account_id, sum(CASE
           WHEN status = 'active' THEN tokens
           WHEN status = 'captured' AND settled_at IS NULL THEN captured_tokens
           ELSE 0 END) AS tokens
  FROM ledger.holds WHERE tokens IS NOT NULL GROUP BY account_id
)
SELECT b.account_id, a.tenant_id, a.currency, b.held_tokens::text AS actual, coalesce(h.tokens, 0)::text AS expected
FROM ledger.balances b
JOIN ledger.accounts a ON a.id = b.account_id
LEFT JOIN held h ON h.account_id = b.account_id
WHERE b.held_tokens <> coalesce(h.tokens, 0)
ORDER BY b.account_id;

-- name: RebuildLotRemainingMismatches :many
WITH used AS (
  SELECT lot_id, sum(tokens) AS tokens FROM ledger.lot_consumptions GROUP BY lot_id
)
SELECT l.id AS lot_id, l.account_id, l.tenant_id, l.tokens_remaining::text AS actual,
       (l.tokens_bought - coalesce(u.tokens, 0))::text AS expected
FROM ledger.token_lots l
LEFT JOIN used u ON u.lot_id = l.id
WHERE l.tokens_remaining <> l.tokens_bought - coalesce(u.tokens, 0)
ORDER BY l.id;

-- name: RebuildWalletValueMismatches :many
-- A service wallet's money is its lots' cost less what was drawn
-- (cumulative rounding: a lot's draws add up to its cost, OQ-15).
WITH lots AS (
  SELECT l.account_id,
         sum(round(l.tokens_bought * l.minor_per_token)
             - coalesce((SELECT sum(c.value_minor) FROM ledger.lot_consumptions c WHERE c.lot_id = l.id), 0)) AS value
  FROM ledger.token_lots l GROUP BY l.account_id
)
SELECT a.id AS account_id, a.tenant_id, a.currency, b.balance_minor AS actual, coalesce(v.value, 0)::bigint AS expected
FROM ledger.accounts a
JOIN ledger.balances b ON b.account_id = a.id
LEFT JOIN lots v ON v.account_id = a.id
WHERE a.type = 'service_wallet' AND b.balance_minor <> coalesce(v.value, 0)
ORDER BY a.id;

-- name: RebuildWalletFloorBreaches :many
-- The wallet guard's rule (3.2): no client wallet below zero, held
-- above its balance, or holding more tokens than its lots have left.
SELECT a.id AS account_id, a.tenant_id, a.currency, b.balance_minor, b.held_minor, b.held_tokens::text AS held_tokens,
       coalesce((SELECT sum(l.tokens_remaining) FROM ledger.token_lots l WHERE l.account_id = a.id), 0)::text AS lots_remaining
FROM ledger.accounts a
JOIN ledger.balances b ON b.account_id = a.id
WHERE ledger.is_client_wallet(a.type)
  AND (b.balance_minor < 0 OR b.held_minor > b.balance_minor
       OR b.held_tokens > coalesce((SELECT sum(l.tokens_remaining) FROM ledger.token_lots l WHERE l.account_id = a.id), 0))
ORDER BY a.id;

-- name: CountRebuildScope :one
SELECT (SELECT count(*) FROM ledger.accounts)::bigint AS accounts,
       (SELECT count(*) FROM ledger.transactions)::bigint AS transactions;

-- name: ClaimRebuildRun :one
-- The replica that inserts the date's row runs it. A row still running
-- an hour after it started belongs to a run that died, and is taken
-- over; otherwise nothing comes back and this replica skips.
INSERT INTO ledger.rebuild_runs (id, run_date, status, started_at)
VALUES (@id, @run_date, 'running', @started_at)
ON CONFLICT (run_date) DO UPDATE SET started_at = excluded.started_at
WHERE ledger.rebuild_runs.status = 'running'
  AND ledger.rebuild_runs.started_at < excluded.started_at - interval '1 hour'
RETURNING *;

-- name: FinishRebuildRun :one
-- Only the run that claimed it (same started_at) finishes it, once.
UPDATE ledger.rebuild_runs
SET status = @status, finished_at = @finished_at, accounts_checked = @accounts_checked,
    transactions_checked = @transactions_checked, differences = @differences
WHERE id = @id AND started_at = @started_at AND status = 'running'
RETURNING *;
