-- name: GetTaxRate :one
SELECT * FROM ledger.tax_rates WHERE country_code = $1 AND tax_code = $2 AND effective_from = $3;
