-- UpsertPreferenceLink issues a link for a number, replacing any earlier
-- one (one per client and number).
-- name: UpsertPreferenceLink :one
INSERT INTO contacts.preference_links (tenant_id, phone_e164, token_hash, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, phone_e164) DO UPDATE
SET token_hash = excluded.token_hash, created_at = excluded.created_at, expires_at = excluded.expires_at, last_used_at = NULL
RETURNING *;

-- FindPreferenceLink finds a link by its token's hash, across clients:
-- run as contacts_system, for the public page.
-- name: FindPreferenceLink :one
SELECT * FROM contacts.preference_links WHERE token_hash = $1;

-- TouchPreferenceLink stamps a link's use, as contacts_system.
-- name: TouchPreferenceLink :exec
UPDATE contacts.preference_links SET last_used_at = sqlc.arg(at) WHERE token_hash = sqlc.arg(token_hash);
