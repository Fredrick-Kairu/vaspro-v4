-- name: CreateKycCheck :one
INSERT INTO accounts.kyc_checks
  (id, tenant_id, kind, subject_id, provider, provider_ref, status)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetKycCheckByID :one
SELECT * FROM accounts.kyc_checks WHERE id = $1 AND tenant_id = $2;
