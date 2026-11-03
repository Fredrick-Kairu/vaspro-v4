-- name: GrantStaff :exec
-- Granting someone who is already staff changes nothing.
INSERT INTO identity.staff_members (user_id, granted_by) VALUES ($1, $2)
ON CONFLICT (user_id) DO NOTHING;

-- name: RevokeStaff :execrows
DELETE FROM identity.staff_members WHERE user_id = $1;

-- name: IsStaff :one
SELECT EXISTS (SELECT 1 FROM identity.staff_members WHERE user_id = $1);
