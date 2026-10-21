-- name: CreateUserTOTP :one
INSERT INTO identity.user_totp (user_id, secret_ciphertext)
VALUES ($1, $2)
RETURNING *;

-- name: GetUserTOTP :one
SELECT * FROM identity.user_totp WHERE user_id = $1;
