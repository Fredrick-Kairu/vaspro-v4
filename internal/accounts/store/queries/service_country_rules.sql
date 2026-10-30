-- name: CreateServiceCountryRule :one
INSERT INTO accounts.service_country_rules (service_code, country_code, min_tier, enabled)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetServiceCountryRule :one
SELECT * FROM accounts.service_country_rules WHERE service_code = $1 AND country_code = $2;
