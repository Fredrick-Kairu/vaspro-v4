-- name: CreateAutoBuyRule :one
INSERT INTO ledger.auto_buy_rules
  (id, tenant_id, service_code, currency, below_tokens, buy_tokens, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetAutoBuyRule :one
SELECT * FROM ledger.auto_buy_rules WHERE tenant_id = $1 AND service_code = $2 AND currency = $3;

-- name: UpsertAutoBuyRule :one
-- One rule per tenant, service and currency; setting it again changes
-- it (task 3.6).
INSERT INTO ledger.auto_buy_rules (id, tenant_id, service_code, currency, below_tokens, buy_tokens, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, service_code, currency)
DO UPDATE SET below_tokens = EXCLUDED.below_tokens, buy_tokens = EXCLUDED.buy_tokens, enabled = EXCLUDED.enabled
RETURNING *;

-- name: ListAutoBuyRules :many
SELECT * FROM ledger.auto_buy_rules WHERE tenant_id = $1 ORDER BY service_code, currency;

-- name: MarkAutoBuyTriggered :exec
UPDATE ledger.auto_buy_rules SET last_triggered_at = $2 WHERE id = $1;
