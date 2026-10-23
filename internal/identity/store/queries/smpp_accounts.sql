-- name: CreateSMPPAccount :one
INSERT INTO identity.smpp_accounts
  (id, tenant_id, system_id, password_hash, ip_allowlist, max_binds, max_tps, environment, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetSMPPAccountBySystemID :one
SELECT * FROM identity.smpp_accounts WHERE system_id = $1;

-- name: GetSMPPAccountByID :one
-- Ownership-checked in the query itself, not just trusted to RLS —
-- same "belt and suspenders" style api_clients.sql already uses.
SELECT * FROM identity.smpp_accounts WHERE id = $1 AND tenant_id = $2;

-- name: ListSMPPAccountsByTenant :many
-- smpp_accounts has no created_at column (data model §3's literal
-- schema) — id is a UUIDv7 (platform/ids), which sorts lexicographically
-- in creation order, so ordering by it gives the same "newest first"
-- result api_clients.sql gets from an explicit created_at column.
SELECT * FROM identity.smpp_accounts WHERE tenant_id = $1 ORDER BY id DESC;

-- name: UpdateSMPPAccountPassword :execrows
UPDATE identity.smpp_accounts SET password_hash = $3
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateSMPPAccountStatus :execrows
UPDATE identity.smpp_accounts SET status = $3
WHERE id = $1 AND tenant_id = $2;
