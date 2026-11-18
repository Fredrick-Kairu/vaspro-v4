-- Starter queries (task 6.1): a create and a lookup per table. Tasks 6.2
-- to 6.5 add what their use cases need.

-- name: CreateContact :one
INSERT INTO contacts.contacts (id, tenant_id, phone_e164, first_name, last_name, email, attributes, source, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetContact :one
SELECT * FROM contacts.contacts WHERE id = $1;

-- name: GetContactByPhone :one
SELECT * FROM contacts.contacts WHERE phone_e164 = $1;

-- InsertContactIfNew adds a contact; a number already in the account is
-- pgx.ErrNoRows, and MergeContact then merges into it.
-- name: InsertContactIfNew :one
INSERT INTO contacts.contacts (id, tenant_id, phone_e164, first_name, last_name, email, attributes, source, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (tenant_id, phone_e164) DO NOTHING
RETURNING *;

-- MergeContact merges into the contact with a number (data model §10):
-- a field given replaces the stored one, attributes merge key by key.
-- name: MergeContact :one
UPDATE contacts.contacts
SET first_name = coalesce(sqlc.narg(first_name), first_name),
    last_name = coalesce(sqlc.narg(last_name), last_name),
    email = coalesce(sqlc.narg(email), email),
    attributes = attributes || sqlc.arg(attributes)::jsonb,
    updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE phone_e164 = sqlc.arg(phone_e164)
RETURNING *;

-- UpdateContact is a PATCH: a field is set only when its set_ flag is
-- (NULL clears it); attributes given are set, and remove_keys removed.
-- name: UpdateContact :one
UPDATE contacts.contacts
SET first_name = CASE WHEN sqlc.arg(set_first_name)::bool THEN sqlc.narg(first_name) ELSE first_name END,
    last_name = CASE WHEN sqlc.arg(set_last_name)::bool THEN sqlc.narg(last_name) ELSE last_name END,
    email = CASE WHEN sqlc.arg(set_email)::bool THEN sqlc.narg(email) ELSE email END,
    attributes = (attributes || sqlc.arg(set_attributes)::jsonb) - sqlc.arg(remove_keys)::text[],
    updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteContact :execrows
DELETE FROM contacts.contacts WHERE id = $1;

-- ListContacts pages newest first by (created_at, id); group_id, when
-- set, keeps one group's contacts.
-- name: ListContacts :many
SELECT c.* FROM contacts.contacts c
WHERE (sqlc.narg(group_id)::uuid IS NULL
       OR EXISTS (SELECT 1 FROM contacts.group_members m WHERE m.contact_id = c.id AND m.group_id = sqlc.narg(group_id)::uuid))
  AND (sqlc.narg(after_created)::timestamptz IS NULL
       OR (c.created_at, c.id) < (sqlc.narg(after_created)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY c.created_at DESC, c.id DESC
LIMIT sqlc.arg(page_size);

-- name: CountContactsByIDs :one
SELECT count(*) FROM contacts.contacts WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- ImportContacts writes one batch of an import (data model §10), given
-- as a JSON array of {id, phone, first_name, last_name, email,
-- attributes}: numbers are unique within the batch; one already in the
-- account merges, a non-empty cell replacing the stored value and
-- attributes merging.
-- name: ImportContacts :execrows
INSERT INTO contacts.contacts AS c (id, tenant_id, phone_e164, first_name, last_name, email, attributes, source, created_by)
SELECT u.id, sqlc.arg(tenant_id)::uuid, u.phone, nullif(u.first_name, ''), nullif(u.last_name, ''), nullif(u.email, ''),
       coalesce(u.attributes, '{}'), 'import', sqlc.arg(created_by)::uuid
FROM jsonb_to_recordset(sqlc.arg(batch)::jsonb)
     AS u(id uuid, phone text, first_name text, last_name text, email text, attributes jsonb)
ON CONFLICT (tenant_id, phone_e164) DO UPDATE
SET first_name = coalesce(excluded.first_name, c.first_name),
    last_name = coalesce(excluded.last_name, c.last_name),
    email = coalesce(excluded.email, c.email),
    attributes = c.attributes || excluded.attributes,
    updated_at = sqlc.arg(at)::timestamptz, version = c.version + 1;

-- name: AddContactsToGroupByPhone :execrows
INSERT INTO contacts.group_members (tenant_id, group_id, contact_id)
SELECT c.tenant_id, sqlc.arg(group_id)::uuid, c.id FROM contacts.contacts c
WHERE c.phone_e164 = ANY(sqlc.arg(phones)::text[])
ON CONFLICT DO NOTHING;
