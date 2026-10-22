-- name: CreateSecurityEvent :one
INSERT INTO identity.security_events (id, user_id, tenant_id, type, ip, detail)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListSecurityEventsByUser :many
SELECT * FROM identity.security_events WHERE user_id = $1 ORDER BY occurred_at DESC;

-- name: CountRecentSignInFailures :one
-- Drives sign-in lockout as a rolling window, not a persisted "locked"
-- state: once a failure ages out of the window, the count — and the
-- lockout — drops on its own ("unlock by time" for free).
SELECT count(*) FROM identity.security_events
WHERE user_id = $1 AND type = 'sign_in_failed' AND occurred_at > $2;
