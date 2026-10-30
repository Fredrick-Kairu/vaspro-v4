-- name: CreateTenantService :one
INSERT INTO accounts.tenant_services (tenant_id, service_code, status)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetTenantService :one
SELECT * FROM accounts.tenant_services WHERE tenant_id = $1 AND service_code = $2;
