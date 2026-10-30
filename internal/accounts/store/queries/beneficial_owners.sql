-- name: CreateBeneficialOwner :one
INSERT INTO accounts.beneficial_owners
  (id, tenant_id, business_profile_id, full_name, role, ownership_pct)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetBeneficialOwnerByID :one
SELECT * FROM accounts.beneficial_owners WHERE id = $1 AND tenant_id = $2;
