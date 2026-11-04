-- name: CreateTierCap :one
INSERT INTO accounts.tier_caps (country_code, tier, metric, period, value)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetTierCap :one
SELECT * FROM accounts.tier_caps
WHERE country_code = $1 AND tier = $2 AND metric = $3 AND period = $4;
