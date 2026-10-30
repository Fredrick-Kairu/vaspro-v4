-- name: CreateBusinessProfile :one
INSERT INTO accounts.business_profiles
  (id, tenant_id, registry, registration_number, registered_name, tax_id, registered_address)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetBusinessProfileByTenant :one
SELECT * FROM accounts.business_profiles WHERE tenant_id = $1;
