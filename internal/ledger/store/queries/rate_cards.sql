-- name: CreateRateCard :one
INSERT INTO ledger.rate_cards
  (id, tenant_id, service_code, country_code, operator_code, variant, tokens_per_event, prices_include_tax, effective_from, effective_to)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetRateCard :one
SELECT * FROM ledger.rate_cards WHERE id = $1;
