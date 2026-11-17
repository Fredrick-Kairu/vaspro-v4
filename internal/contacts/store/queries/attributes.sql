-- name: CreateAttributeDefinition :one
INSERT INTO contacts.attribute_definitions (tenant_id, key, label, type)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListAttributeDefinitions :many
SELECT * FROM contacts.attribute_definitions ORDER BY key;

-- name: CountAttributeDefinitions :one
SELECT count(*) FROM contacts.attribute_definitions;

-- name: DeleteAttributeDefinition :execrows
DELETE FROM contacts.attribute_definitions WHERE key = $1;

-- name: RemoveAttributeFromContacts :execrows
UPDATE contacts.contacts SET attributes = attributes - sqlc.arg(key)::text, updated_at = sqlc.arg(at)::timestamptz,
  version = version + 1
WHERE attributes ? sqlc.arg(key)::text;
