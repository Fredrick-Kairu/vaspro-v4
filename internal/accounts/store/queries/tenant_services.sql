-- name: CreateTenantService :one
INSERT INTO accounts.tenant_services (tenant_id, service_code, status)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetTenantService :one
SELECT * FROM accounts.tenant_services WHERE tenant_id = $1 AND service_code = $2;

-- name: ListTenantServices :many
SELECT * FROM accounts.tenant_services WHERE tenant_id = $1 ORDER BY service_code;

-- name: SetTenantServiceStatus :exec
-- enabled_at is set on enabling and cleared on locking.
UPDATE accounts.tenant_services
SET status = @status,
    enabled_at = CASE WHEN @status::text = 'enabled' THEN now() ELSE NULL END
WHERE tenant_id = @tenant_id AND service_code = @service_code;

-- name: DeleteTenantService :execrows
DELETE FROM accounts.tenant_services WHERE tenant_id = $1 AND service_code = $2;
