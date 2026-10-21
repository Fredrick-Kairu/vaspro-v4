-- name: CreateVerificationToken :one
INSERT INTO identity.verification_tokens (id, user_id, purpose, target, token_hash, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetVerificationTokenByHash :one
SELECT * FROM identity.verification_tokens WHERE token_hash = $1;

-- name: GetPendingVerificationTokenByTarget :one
-- verification_tokens has no created_at (data model §3's own column
-- list) — DeletePendingVerificationTokens always clears any prior
-- pending row before a new one is issued, so at most one should exist;
-- ordering by expires_at DESC is just a defensive tie-breaker.
SELECT * FROM identity.verification_tokens
WHERE target = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > now()
ORDER BY expires_at DESC
LIMIT 1;

-- name: DeletePendingVerificationTokens :exec
DELETE FROM identity.verification_tokens
WHERE target = $1 AND purpose = $2 AND used_at IS NULL;

-- name: IncrementVerificationTokenAttempts :exec
UPDATE identity.verification_tokens SET attempts = attempts + 1 WHERE id = $1;

-- name: MarkVerificationTokenUsed :exec
UPDATE identity.verification_tokens SET used_at = now() WHERE id = $1;
