-- AddSuppression adds a number to the client's list; one already there
-- (for that channel, or every channel when channel is NULL) is
-- pgx.ErrNoRows.
-- name: AddSuppression :one
INSERT INTO contacts.suppressions (tenant_id, phone_e164, channel, reason, note, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, phone_e164, channel) DO NOTHING
RETURNING *;

-- name: ListSuppressionsForPhone :many
SELECT * FROM contacts.suppressions WHERE phone_e164 = $1 ORDER BY created_at;

-- name: GetSuppression :one
SELECT * FROM contacts.suppressions
WHERE phone_e164 = sqlc.arg(phone_e164) AND channel IS NOT DISTINCT FROM sqlc.narg(channel)::contacts.channel;

-- name: DeleteSuppression :execrows
DELETE FROM contacts.suppressions
WHERE phone_e164 = sqlc.arg(phone_e164) AND channel IS NOT DISTINCT FROM sqlc.narg(channel)::contacts.channel;

-- ListSuppressions pages the client's list by number, then channel
-- (every channel first).
-- name: ListSuppressions :many
SELECT * FROM contacts.suppressions
WHERE sqlc.narg(after_phone)::text IS NULL
   OR (phone_e164, coalesce(channel::text, '')) > (sqlc.narg(after_phone)::text, sqlc.arg(after_channel)::text)
ORDER BY phone_e164, coalesce(channel::text, '')
LIMIT sqlc.arg(page_size);
