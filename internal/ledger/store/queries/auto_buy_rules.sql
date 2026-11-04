-- name: CreateAutoBuyRule :one
INSERT INTO ledger.auto_buy_rules
  (id, tenant_id, service_code, currency, below_tokens, buy_tokens, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetAutoBuyRule :one
SELECT * FROM ledger.auto_buy_rules WHERE tenant_id = $1 AND service_code = $2 AND currency = $3;
