-- name: CreateKycCheck :one
INSERT INTO accounts.kyc_checks
  (id, tenant_id, kind, subject_id, provider, provider_ref, status)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetKycCheckByID :one
SELECT * FROM accounts.kyc_checks WHERE id = $1 AND tenant_id = $2;

-- name: FindKycCheckByProviderRef :one
-- Only under the accounts_kyc_webhook role (app.HandleKYCWebhook): a
-- webhook names the check, not the account.
SELECT id, tenant_id FROM accounts.kyc_checks WHERE provider = $1 AND provider_ref = $2;

-- name: GetKycCheckForUpdate :one
SELECT * FROM accounts.kyc_checks WHERE id = $1 AND tenant_id = $2 FOR UPDATE;

-- name: DecideKycCheck :exec
-- decided_at is set for a decision and left empty for review.
UPDATE accounts.kyc_checks
SET status = @status, result = @result,
    decided_at = CASE WHEN @status::text = 'review' THEN NULL ELSE now() END
WHERE id = @id AND tenant_id = @tenant_id;

-- name: ListKycChecks :many
SELECT * FROM accounts.kyc_checks WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC;
