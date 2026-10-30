-- name: CreateDocument :one
INSERT INTO accounts.documents
  (id, tenant_id, type, storage_key, sha256, mime_type, size_bytes, status, expires_at, uploaded_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetDocumentByID :one
SELECT * FROM accounts.documents WHERE id = $1 AND tenant_id = $2;
