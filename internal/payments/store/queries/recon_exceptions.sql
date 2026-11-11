-- name: CreateReconException :one
INSERT INTO payments.recon_exceptions (id, tenant_id, rail_account_id, kind, statement_line_id, reference_type, reference_id, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'open')
RETURNING *;

-- name: GetReconException :one
SELECT * FROM payments.recon_exceptions WHERE id = $1;

-- ListReconExceptions is the staff queue (payments_operations): by
-- status ('' for all), oldest first, after an (opened_at, id) cursor.
-- name: ListReconExceptions :many
SELECT * FROM payments.recon_exceptions
WHERE (sqlc.arg(status)::text = '' OR status = sqlc.arg(status)::text)
  AND (opened_at, id) > (sqlc.arg(after_opened_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY opened_at, id
LIMIT sqlc.arg(max_rows);

-- ResolveReconException closes an open exception; a closed one is left.
-- name: ResolveReconException :one
UPDATE payments.recon_exceptions SET status = $2, resolution = $3, resolved_by = $4, resolved_at = $5
WHERE id = $1 AND status = 'open'
RETURNING *;
