-- name: UpsertRail :one
INSERT INTO payments.rails (code, country_code, kind, capabilities, status)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (code) DO UPDATE SET country_code = EXCLUDED.country_code, kind = EXCLUDED.kind,
  capabilities = EXCLUDED.capabilities, status = EXCLUDED.status
RETURNING *;

-- name: GetRail :one
SELECT * FROM payments.rails WHERE code = $1;

-- RetireRailsNotIn retires every rail no pack lists any more. Rails are
-- never deleted: rail accounts reference them.
-- name: RetireRailsNotIn :execrows
UPDATE payments.rails SET status = 'retired'
WHERE NOT (code = ANY (sqlc.arg(codes)::text[])) AND status <> 'retired';
