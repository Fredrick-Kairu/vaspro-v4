-- name: CreateReversal :one
INSERT INTO ledger.reversals
  (id, tenant_id, original_transaction_id, amount_minor, reason, evidence_document_id, status, requested_by,
   kind, currency, clawback_services)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetReversal :one
SELECT * FROM ledger.reversals WHERE id = $1;

-- name: LockReversal :one
SELECT * FROM ledger.reversals WHERE id = $1 FOR UPDATE;

-- name: SumLiveReversals :one
-- What the top-up's reversals take already (task 3.7): every one that
-- wasn't rejected. The caller holds the main wallet's balance row lock,
-- which serialises reversals of the tenant's top-ups.
SELECT coalesce(sum(amount_minor), 0)::bigint FROM ledger.reversals
WHERE original_transaction_id = $1 AND status <> 'rejected';

-- name: ApproveReversal :one
UPDATE ledger.reversals
SET status = @status, approved_by = @approved_by, decided_at = @decided_at,
    reversal_transaction_id = @reversal_transaction_id
WHERE id = @id
RETURNING *;

-- name: RejectReversal :one
UPDATE ledger.reversals
SET status = 'rejected', rejected_by = @rejected_by, rejection_reason = @rejection_reason, decided_at = @decided_at
WHERE id = @id
RETURNING *;

-- name: MarkReversalPaidBack :one
UPDATE ledger.reversals
SET status = 'paid_back', payback_transaction_id = @payback_transaction_id, payback_reference = @payback_reference
WHERE id = @id
RETURNING *;

-- name: RecordReversalPaybackFailure :one
-- A failed pay-back leaves the reversal pending, the money still in
-- reversal_clearing, for the rail's retry (system design §4.3).
UPDATE ledger.reversals
SET payback_attempts = payback_attempts + 1, last_payback_error = @last_payback_error,
    payback_reference = coalesce(sqlc.narg(payback_reference), payback_reference)
WHERE id = @id
RETURNING *;

-- name: ListReversalsPage :many
-- Staff's queue across clients (task 3.7), run as
-- ledger_reversal_review: oldest first (ids are UUIDv7), filtered,
-- after a cursor.
SELECT * FROM ledger.reversals
WHERE (sqlc.narg(tenant_id)::uuid IS NULL OR tenant_id = sqlc.narg(tenant_id))
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(after)::uuid IS NULL OR id > sqlc.narg(after))
ORDER BY id
LIMIT sqlc.arg(max_rows);

-- name: GetReversalTenant :one
-- Which client a reversal belongs to, read as ledger_reversal_review so
-- staff can then act in that client's own transaction.
SELECT tenant_id FROM ledger.reversals WHERE id = $1;
