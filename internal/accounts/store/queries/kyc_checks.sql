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

-- name: CreateKycCheckWithResult :one
-- A check decided or sent to review when it's created (a manual KYB
-- submission, task 2.7).
INSERT INTO accounts.kyc_checks (id, tenant_id, kind, subject_id, provider, status, result)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: LatestKycCheckOfKind :one
SELECT * FROM accounts.kyc_checks
WHERE tenant_id = $1 AND kind = $2
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListKycReviews :many
-- Only under the accounts_staff_review role (task 2.9): the staff queue,
-- oldest first, across accounts, after an optional (created_at, id)
-- cursor. Uses kyc_checks_review.
SELECT c.id, c.tenant_id, c.kind, c.result, c.created_at,
       t.display_name, t.country_code, t.tier, t.status AS tenant_status
FROM accounts.kyc_checks c
JOIN accounts.tenants t ON t.id = c.tenant_id
WHERE c.status = 'review'
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (c.created_at, c.id) > (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY c.created_at, c.id
LIMIT sqlc.arg('max_rows');

-- name: FindKycCheckTenant :one
-- Only under the accounts_staff_review role: which account a check
-- belongs to, so the staff action can run in that account's transaction.
SELECT tenant_id FROM accounts.kyc_checks WHERE id = $1;

-- name: DecideKycCheckByStaff :exec
-- A staff decision (task 2.9): the decision, the reviewer, and extra
-- result fields merged over what the check already recorded.
UPDATE accounts.kyc_checks
SET status = @status, result = result || @extra::jsonb, reviewed_by = @reviewed_by, decided_at = now()
WHERE id = @id AND tenant_id = @tenant_id;
