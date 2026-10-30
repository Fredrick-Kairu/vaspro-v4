-- name: CreateService :one
INSERT INTO accounts.services (code, name, default_min_tier)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetServiceByCode :one
SELECT * FROM accounts.services WHERE code = $1;
