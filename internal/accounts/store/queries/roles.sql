-- name: CreateRole :one
INSERT INTO accounts.roles (id, tenant_id, name, permissions)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetRoleByID :one
SELECT * FROM accounts.roles WHERE id = $1;
