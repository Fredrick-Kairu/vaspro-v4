-- name: CreateSegment :one
INSERT INTO contacts.segments (id, tenant_id, name, filter, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetSegment :one
SELECT * FROM contacts.segments WHERE id = $1;

-- name: ListSegments :many
SELECT * FROM contacts.segments ORDER BY lower(name), id;

-- name: UpdateSegment :one
UPDATE contacts.segments
SET name = coalesce(sqlc.narg(name), name),
    filter = coalesce(sqlc.narg(filter)::jsonb, filter),
    updated_at = sqlc.arg(at), version = version + 1
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteSegment :execrows
DELETE FROM contacts.segments WHERE id = $1;

-- name: CountGroupsByIDs :one
SELECT count(*) FROM contacts.groups WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- The two queries below are the one fixed query that evaluates every
-- segment (data model §10): each contact becomes a JSON document of its
-- fields (email lower-cased, created_at as a date in the account's time
-- zone), attributes and, when the filter needs them, group IDs, and the
-- compiled path is matched against it with its variables. Nothing in
-- the SQL comes from the client. Counting reads every contact, so it
-- takes group IDs from one grouped join (0.8s rather than 3.2s on a
-- million contacts, task 6.3); a page stops after its matches, so it
-- looks them up per contact (5ms rather than 0.8s for the first page).

-- name: CountSegmentContacts :one
SELECT count(*) FROM contacts.contacts c
LEFT JOIN (SELECT m.contact_id, jsonb_agg(m.group_id) AS ids FROM contacts.group_members m
           WHERE sqlc.arg(with_groups)::bool AND m.tenant_id = sqlc.arg(tenant_id) GROUP BY m.contact_id) g ON g.contact_id = c.id
WHERE c.tenant_id = sqlc.arg(tenant_id)
  AND jsonb_path_match(
        jsonb_strip_nulls(jsonb_build_object(
          'first_name', c.first_name, 'last_name', c.last_name, 'email', lower(c.email::text),
          'created_at', to_char(c.created_at AT TIME ZONE sqlc.arg(zone)::text, 'YYYY-MM-DD'),
          'attributes', c.attributes,
          'groups', g.ids)),
        sqlc.arg(path)::text::jsonpath, sqlc.arg(vars)::jsonb, true);

-- name: ListSegmentContacts :many
SELECT c.* FROM contacts.contacts c
WHERE c.tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(after_created)::timestamptz IS NULL
       OR (c.created_at, c.id) < (sqlc.narg(after_created)::timestamptz, sqlc.narg(after_id)::uuid))
  AND jsonb_path_match(
        jsonb_strip_nulls(jsonb_build_object(
          'first_name', c.first_name, 'last_name', c.last_name, 'email', lower(c.email::text),
          'created_at', to_char(c.created_at AT TIME ZONE sqlc.arg(zone)::text, 'YYYY-MM-DD'),
          'attributes', c.attributes,
          'groups', CASE WHEN sqlc.arg(with_groups)::bool THEN
                      (SELECT jsonb_agg(m.group_id) FROM contacts.group_members m WHERE m.tenant_id = c.tenant_id AND m.contact_id = c.id)
                    END)),
        sqlc.arg(path)::text::jsonpath, sqlc.arg(vars)::jsonb, true)
ORDER BY c.created_at DESC, c.id DESC
LIMIT sqlc.arg(page_size);
