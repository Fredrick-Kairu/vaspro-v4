-- name: CreateUser :one
INSERT INTO identity.users (id, email, full_name, password_hash)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM identity.users WHERE email = $1;
