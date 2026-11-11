-- name: UpsertRail :one
INSERT INTO payments.rails (code, country_code, kind, capabilities, status)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (code) DO UPDATE SET country_code = EXCLUDED.country_code, kind = EXCLUDED.kind,
  capabilities = EXCLUDED.capabilities, status = EXCLUDED.status
RETURNING *;

-- name: GetRail :one
SELECT * FROM payments.rails WHERE code = $1;
