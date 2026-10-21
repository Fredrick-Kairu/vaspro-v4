-- name: CreateVerificationToken :one
INSERT INTO identity.verification_tokens (id, user_id, purpose, target, token_hash, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetVerificationTokenByHash :one
SELECT * FROM identity.verification_tokens WHERE token_hash = $1;
