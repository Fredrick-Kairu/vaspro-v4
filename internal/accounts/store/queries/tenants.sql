-- name: CreateTenant :one
INSERT INTO accounts.tenants
  (id, type, parent_tenant_id, legal_name, display_name, country_code, timezone, default_currency, region, billing_email)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetTenantByID :one
SELECT * FROM accounts.tenants WHERE id = $1;
