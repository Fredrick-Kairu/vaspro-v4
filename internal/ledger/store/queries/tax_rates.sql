-- name: GetTaxRate :one
SELECT * FROM ledger.tax_rates WHERE country_code = $1 AND tax_code = $2 AND effective_from = $3;

-- name: CreateTaxRateIfNew :exec
-- The startup sync from the country packs (task 3.5): a rate is never
-- edited; a new rate is a new row from a new date.
INSERT INTO ledger.tax_rates (country_code, tax_code, rate, effective_from)
VALUES ($1, $2, $3, $4)
ON CONFLICT (country_code, tax_code, effective_from) DO NOTHING;

-- name: GetTaxRateAt :one
-- The rate in force on a date: the latest one from on or before it.
SELECT * FROM ledger.tax_rates
WHERE country_code = $1 AND tax_code = $2 AND effective_from <= $3
  AND (effective_to IS NULL OR effective_to > $3)
ORDER BY effective_from DESC
LIMIT 1;
