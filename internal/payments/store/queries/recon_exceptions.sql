-- name: CreateReconException :one
INSERT INTO payments.recon_exceptions (id, tenant_id, rail_account_id, kind, statement_line_id, reference_type, reference_id, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'open')
RETURNING *;

-- name: GetReconException :one
SELECT * FROM payments.recon_exceptions WHERE id = $1;
