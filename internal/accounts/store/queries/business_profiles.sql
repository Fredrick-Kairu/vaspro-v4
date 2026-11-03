-- name: CreateBusinessProfile :one
INSERT INTO accounts.business_profiles
  (id, tenant_id, registry, registration_number, registered_name, tax_id, registered_address)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetBusinessProfileByTenant :one
SELECT * FROM accounts.business_profiles WHERE tenant_id = $1;

-- name: UpdateBusinessProfile :one
UPDATE accounts.business_profiles
SET registration_number = $2, registered_name = $3, tax_id = $4, registered_address = $5
WHERE tenant_id = $1
RETURNING *;

-- name: CountOtherProfilesWithRegistration :one
-- Only under the accounts_registration_lookup role (app.SubmitKYB):
-- data model §4 flags, not blocks, a registration number another
-- account also uses.
SELECT count(*) FROM accounts.business_profiles
WHERE registry = $1 AND registration_number = $2 AND tenant_id <> $3;

-- name: SetBusinessProfileVerified :exec
-- Set when staff pass the business check (task 2.9).
UPDATE accounts.business_profiles SET verified_at = now() WHERE id = $1 AND tenant_id = $2;
