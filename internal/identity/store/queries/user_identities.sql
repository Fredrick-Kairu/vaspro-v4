-- name: CreateUserIdentity :one
INSERT INTO identity.user_identities (id, user_id, provider, subject, email_at_link, hosted_domain)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetUserIdentityBySubject :one
-- Google accounts are matched on the provider's sub, never on email
-- (D-22), so a changed Google email still finds the same user.
SELECT * FROM identity.user_identities WHERE provider = $1 AND subject = $2;

-- name: GetUserIdentityByUser :one
SELECT * FROM identity.user_identities WHERE user_id = $1 AND provider = $2;

-- name: TouchUserIdentity :exec
UPDATE identity.user_identities SET last_used_at = now() WHERE id = $1;

-- name: DeleteUserIdentity :execrows
DELETE FROM identity.user_identities WHERE user_id = $1 AND provider = $2;
