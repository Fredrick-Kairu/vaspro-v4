-- name: CreateReversal :one
INSERT INTO ledger.reversals
  (id, tenant_id, original_transaction_id, amount_minor, reason, evidence_document_id, status, requested_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetReversal :one
SELECT * FROM ledger.reversals WHERE id = $1;
