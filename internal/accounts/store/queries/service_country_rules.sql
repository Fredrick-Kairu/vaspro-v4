-- name: CreateServiceCountryRule :one
INSERT INTO accounts.service_country_rules (service_code, country_code, min_tier, enabled)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetServiceCountryRule :one
SELECT * FROM accounts.service_country_rules WHERE service_code = $1 AND country_code = $2;

-- name: UpsertServiceCountryRule :exec
INSERT INTO accounts.service_country_rules (service_code, country_code, min_tier, enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (service_code, country_code) DO UPDATE
SET min_tier = EXCLUDED.min_tier, enabled = EXCLUDED.enabled;

-- name: DeleteServiceCountryRulesExcept :exec
-- Drops a country's rules for services its pack no longer lists.
DELETE FROM accounts.service_country_rules
WHERE country_code = @country_code AND NOT (service_code = ANY(@keep::text[]));

-- name: ListServicesForCountry :many
-- Every catalogue service with its effective rule in a country: the
-- pack's rule if there is one, otherwise the catalogue default, enabled.
SELECT s.code, s.name,
       COALESCE(r.min_tier, s.default_min_tier)::accounts.tier AS min_tier,
       COALESCE(r.enabled, true)::boolean AS enabled
FROM accounts.services s
LEFT JOIN accounts.service_country_rules r
  ON r.service_code = s.code AND r.country_code = @country_code
ORDER BY s.code;
