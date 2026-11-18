-- name: CreateGroup :one
INSERT INTO contacts.groups (id, tenant_id, name, description, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetGroup :one
SELECT * FROM contacts.groups WHERE id = $1;

-- AddGroupMember is repeat-safe.
-- name: AddGroupMember :exec
INSERT INTO contacts.group_members (tenant_id, group_id, contact_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: CountGroupMembers :one
SELECT count(*) FROM contacts.group_members WHERE group_id = $1;

-- name: ListGroups :many
SELECT g.*, (SELECT count(*) FROM contacts.group_members m WHERE m.group_id = g.id) AS members
FROM contacts.groups g ORDER BY lower(g.name);

-- name: GetGroupWithCount :one
SELECT g.*, (SELECT count(*) FROM contacts.group_members m WHERE m.group_id = g.id) AS members
FROM contacts.groups g WHERE g.id = $1;

-- name: UpdateGroup :one
UPDATE contacts.groups
SET name = coalesce(sqlc.narg(name), name),
    description = CASE WHEN sqlc.arg(set_description)::bool THEN sqlc.narg(description) ELSE description END,
    updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteGroup :execrows
DELETE FROM contacts.groups WHERE id = $1;

-- name: AddGroupMembers :execrows
INSERT INTO contacts.group_members (tenant_id, group_id, contact_id)
SELECT sqlc.arg(tenant_id)::uuid, sqlc.arg(group_id)::uuid, unnest(sqlc.arg(contact_ids)::uuid[])
ON CONFLICT DO NOTHING;

-- name: RemoveGroupMember :execrows
DELETE FROM contacts.group_members WHERE group_id = $1 AND contact_id = $2;
