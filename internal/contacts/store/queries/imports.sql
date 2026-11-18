-- name: CreateImport :one
INSERT INTO contacts.imports (id, tenant_id, group_id, file_name, content_type, object_key,
  upload_expires_at, delete_after, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetImport :one
SELECT * FROM contacts.imports WHERE id = $1;

-- name: InsertImportError :exec
INSERT INTO contacts.import_errors (import_id, tenant_id, row_no, field, error_code, message)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT DO NOTHING;

-- name: ListImportErrors :many
SELECT * FROM contacts.import_errors WHERE import_id = $1 ORDER BY row_no, field, error_code;

-- name: GetImportForUpdate :one
SELECT * FROM contacts.imports WHERE id = $1 FOR UPDATE;

-- name: GetImportByObjectKey :one
SELECT * FROM contacts.imports WHERE object_key = $1 OR error_report_key = $1;

-- ListImports pages newest first by (created_at, id).
-- name: ListImports :many
SELECT * FROM contacts.imports
WHERE sqlc.narg(after_created)::timestamptz IS NULL
   OR (created_at, id) < (sqlc.narg(after_created)::timestamptz, sqlc.narg(after_id)::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- ConfirmImportUpload moves an import to scanning, only while it awaits
-- its upload and the form still works.
-- name: ConfirmImportUpload :one
UPDATE contacts.imports SET status = 'scanning', version = version + 1
WHERE id = sqlc.arg(id) AND status = 'awaiting_upload' AND upload_expires_at > sqlc.arg(at)::timestamptz
RETURNING *;

-- name: SetImportScanned :exec
UPDATE contacts.imports SET status = 'validating', size_bytes = $2, sha256 = $3, version = version + 1
WHERE id = $1 AND status = 'scanning';

-- name: SetImportProcessing :exec
UPDATE contacts.imports SET status = 'processing', version = version + 1
WHERE id = $1 AND status = 'validating';

-- AddImportProgress counts a batch in the same transaction as its
-- rows: read is every data row read (blank ones too), so rows_total is
-- also how far a resumed import skips.
-- name: AddImportProgress :exec
UPDATE contacts.imports SET rows_valid = rows_valid + sqlc.arg(valid)::int, rows_failed = rows_failed + sqlc.arg(failed)::int,
  rows_total = coalesce(rows_total, 0) + sqlc.arg(read)::int, version = version + 1
WHERE id = sqlc.arg(id) AND status = 'processing';

-- FinishImport sets a final status; the clock for deleting the files
-- starts now (data model §8).
-- name: FinishImport :one
UPDATE contacts.imports
SET status = sqlc.arg(status), rows_total = coalesce(rows_total, 0), error_report_key = sqlc.narg(error_report_key),
    finished_at = sqlc.arg(at)::timestamptz, delete_after = sqlc.arg(delete_after)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id) AND status IN ('scanning', 'validating', 'processing')
RETURNING *;

-- name: ListImportErrorsPage :many
SELECT * FROM contacts.import_errors
WHERE import_id = sqlc.arg(import_id)
  AND (sqlc.narg(after_row)::int IS NULL
       OR (row_no, field, error_code) > (sqlc.narg(after_row)::int, sqlc.narg(after_field)::text, sqlc.narg(after_code)::text))
ORDER BY row_no, field, error_code
LIMIT sqlc.arg(page_size);

-- name: CountImportErrors :one
SELECT count(*) FROM contacts.import_errors WHERE import_id = $1;

-- The worker's queries, as contacts_system across clients.

-- name: ListImportsToProcess :many
SELECT id, tenant_id FROM contacts.imports
WHERE status IN ('scanning', 'validating', 'processing')
ORDER BY created_at
LIMIT $1;

-- ExpireImports ends imports whose upload form ran out unused; their
-- files (if any) go at the new delete_after.
-- name: ExpireImports :many
UPDATE contacts.imports
SET status = 'expired', finished_at = sqlc.arg(at)::timestamptz, delete_after = sqlc.arg(delete_after)::timestamptz,
    version = version + 1
WHERE status = 'awaiting_upload' AND upload_expires_at <= sqlc.arg(at)::timestamptz
RETURNING *;

-- DueImportFiles lists imports whose files' time is up: never one still
-- being processed.
-- name: DueImportFiles :many
SELECT id, tenant_id, object_key, error_report_key FROM contacts.imports
WHERE deleted_at IS NULL AND delete_after <= sqlc.arg(at)::timestamptz
  AND status NOT IN ('scanning', 'validating', 'processing')
ORDER BY delete_after
LIMIT sqlc.arg(page_size);

-- name: MarkImportFilesDeleted :exec
UPDATE contacts.imports SET deleted_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- Session advisory locks, held on one connection while an import is
-- worked on, so a worker that dies releases it (data model §10).
-- name: TryLockImport :one
SELECT pg_try_advisory_lock(hashtextextended(sqlc.arg(id)::uuid::text, 0));

-- name: UnlockImport :one
SELECT pg_advisory_unlock(hashtextextended(sqlc.arg(id)::uuid::text, 0));
