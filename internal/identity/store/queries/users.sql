-- name: CreateUser :one
INSERT INTO identity.users (id, email, full_name, phone_e164)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM identity.users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM identity.users WHERE id = $1;

-- name: MarkEmailVerified :exec
UPDATE identity.users SET email_verified_at = now(), updated_at = now() WHERE id = $1;

-- name: SetPasswordHash :exec
UPDATE identity.users SET password_hash = $2, updated_at = now() WHERE id = $1;
