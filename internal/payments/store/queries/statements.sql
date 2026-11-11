-- name: CreateStatementImport :one
INSERT INTO payments.statement_imports (id, rail_account_id, tenant_id, period_start, period_end, source, object_key, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'importing')
RETURNING *;

-- name: GetStatementImport :one
SELECT * FROM payments.statement_imports WHERE id = $1;

-- name: CreateStatementLine :one
INSERT INTO payments.statement_lines (import_id, rail_receipt, direction, amount_minor, charges_minor, occurred_at, match_status)
VALUES ($1, $2, $3, $4, $5, $6, 'unmatched')
RETURNING *;

-- name: ListStatementLines :many
SELECT * FROM payments.statement_lines WHERE import_id = $1 ORDER BY occurred_at, id;

-- name: InsertStatementLine :one
INSERT INTO payments.statement_lines (import_id, rail_receipt, direction, amount_minor, occurred_at, matched_type, matched_id, match_status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id;

-- name: FinishStatementImport :exec
UPDATE payments.statement_imports SET status = 'matched', line_count = $2 WHERE id = $1;

-- name: StatementImportExists :one
SELECT EXISTS (SELECT 1 FROM payments.statement_imports WHERE rail_account_id = $1 AND period_start = $2);
