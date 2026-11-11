-- name: GrantStaff :exec
-- Granting someone who is already staff changes nothing.
INSERT INTO identity.staff_members (user_id, granted_by) VALUES ($1, $2)
ON CONFLICT (user_id) DO NOTHING;

-- name: RevokeStaff :execrows
DELETE FROM identity.staff_members WHERE user_id = $1;

-- name: IsStaff :one
SELECT EXISTS (SELECT 1 FROM identity.staff_members WHERE user_id = $1);

-- name: GetStaffRole :one
-- Task 3.7: staff and the finance-lead flag in one read; no row means
-- neither.
SELECT EXISTS (SELECT 1 FROM identity.staff_members s WHERE s.user_id = sqlc.arg(user_id)) AS staff,
       EXISTS (SELECT 1 FROM identity.staff_members s WHERE s.user_id = sqlc.arg(user_id) AND s.finance_lead) AS finance_lead;

-- name: SetFinanceLead :execrows
-- Only an existing staff member's flag changes; a no-op change counts
-- as no row, so callers can tell "already so" from "changed".
UPDATE identity.staff_members SET finance_lead = sqlc.arg(finance_lead)
WHERE user_id = sqlc.arg(user_id) AND finance_lead <> sqlc.arg(finance_lead);
