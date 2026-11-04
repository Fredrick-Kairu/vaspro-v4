-- name: CreateSecurityEvent :one
INSERT INTO identity.security_events (id, user_id, tenant_id, type, ip, detail)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListSecurityEventsByUser :many
-- Bounded at 100, not true cursor pagination (.claude/rules/api.md
-- otherwise wants cursor-based paging) — same "only build what's
-- needed now" call every other Phase 1 list endpoint already makes
-- (none of them paginate yet either); worth revisiting if this table's
-- per-user row count ever makes 100 feel tight.
SELECT * FROM identity.security_events WHERE user_id = $1 ORDER BY occurred_at DESC LIMIT 100;

-- name: CountSignInEventsByUser :one
-- Zero means this sign-in is the account's first ever — task 1.12's
-- own call site treats that as nothing to compare "new IP" against,
-- not an alert-worthy novelty.
SELECT count(*) FROM identity.security_events WHERE user_id = $1 AND type = 'sign_in';

-- name: CountSignInEventsByUserAndIP :one
-- Zero means ip has never appeared on a prior sign_in event for this
-- user — task 1.12's "used from a new IP" check.
SELECT count(*) FROM identity.security_events WHERE user_id = $1 AND type = 'sign_in' AND ip = $2;

-- name: CountRecentSignInFailures :one
-- Drives sign-in lockout as a rolling window, not a persisted "locked"
-- state: once a failure ages out of the window, the count — and the
-- lockout — drops on its own ("unlock by time" for free).
SELECT count(*) FROM identity.security_events
WHERE user_id = $1 AND type = 'sign_in_failed' AND occurred_at > $2;

-- name: LatestStaffUnlockAt :one
-- The most recent unlock by staff (task 2.9); sign-in failures before
-- it no longer count toward the lockout. The epoch when never unlocked,
-- so the caller's window start always wins.
SELECT COALESCE(max(occurred_at), 'epoch'::timestamptz)::timestamptz FROM identity.security_events
WHERE user_id = $1 AND type = 'unlocked_by_staff';
