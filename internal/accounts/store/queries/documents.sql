-- name: CreateDocument :one
INSERT INTO accounts.documents
  (id, tenant_id, type, storage_key, sha256, mime_type, size_bytes, status, expires_at, uploaded_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetDocumentByID :one
SELECT * FROM accounts.documents WHERE id = $1 AND tenant_id = $2;

-- name: GetDocumentBySHA256 :one
SELECT * FROM accounts.documents WHERE tenant_id = $1 AND sha256 = $2;

-- name: ListDocuments :many
SELECT * FROM accounts.documents WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC;

-- name: SetDocumentsStatus :exec
-- A staff decision accepts or rejects the documents a check named (task 2.9).
UPDATE accounts.documents SET status = @status
WHERE tenant_id = @tenant_id AND id = ANY(@ids::uuid[]);
