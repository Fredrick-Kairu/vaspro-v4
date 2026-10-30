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

-- name: CreateV3ImportedUser :one
-- task 1.13: a v3 account already proved control of its email by
-- using it in v3, so email_verified_at is set immediately rather
-- than sending a fresh verification code. ON CONFLICT (email) DO
-- NOTHING, with no RETURNING match on conflict, is this query's own
-- idempotency: a re-run of the importer (or a row already imported
-- in an earlier batch) comes back as pgx.ErrNoRows, which the caller
-- treats as "already imported," not an error.
INSERT INTO identity.users (id, email, full_name, phone_e164, password_hash, email_verified_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (email) DO NOTHING
RETURNING *;

-- name: CreateGoogleUser :one
-- task 1.16: Google has verified the email (email_verified claim), so
-- the account starts verified; there is no password until the user
-- sets one through a password reset.
INSERT INTO identity.users (id, email, full_name, email_verified_at)
VALUES ($1, $2, $3, now())
RETURNING *;
