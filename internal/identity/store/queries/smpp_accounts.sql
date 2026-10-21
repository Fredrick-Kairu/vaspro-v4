-- name: CreateSMPPAccount :one
INSERT INTO identity.smpp_accounts
  (id, tenant_id, system_id, password_hash, ip_allowlist, max_binds, max_tps, environment, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetSMPPAccountBySystemID :one
SELECT * FROM identity.smpp_accounts WHERE system_id = $1;
