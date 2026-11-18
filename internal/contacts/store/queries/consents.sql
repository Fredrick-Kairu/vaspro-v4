-- UpsertConsent records a number's consent for one channel and purpose.
-- A person's own withdrawal can't be overwritten by the client: the
-- trigger refuses it (check_violation).
-- name: UpsertConsent :one
INSERT INTO contacts.consents (tenant_id, phone_e164, channel, purpose, status, actor, source, evidence, recorded_by, recorded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (tenant_id, phone_e164, channel, purpose) DO UPDATE
SET status = excluded.status, actor = excluded.actor, source = excluded.source, evidence = excluded.evidence,
  recorded_by = excluded.recorded_by, recorded_at = excluded.recorded_at, version = contacts.consents.version + 1
RETURNING *;

-- name: GetConsent :one
SELECT * FROM contacts.consents
WHERE phone_e164 = $1 AND channel = $2 AND purpose = $3;

-- name: InsertConsentEvent :exec
INSERT INTO contacts.consent_events (id, tenant_id, phone_e164, channel, purpose, status, actor, source, evidence, recorded_by, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: ListConsentEvents :many
SELECT * FROM contacts.consent_events WHERE phone_e164 = $1 ORDER BY occurred_at DESC, id DESC;

-- name: ListConsentsForPhone :many
SELECT * FROM contacts.consents WHERE phone_e164 = $1 ORDER BY channel, purpose;

-- GetConsentForUpdate locks one consent row while it's changed.
-- name: GetConsentForUpdate :one
SELECT * FROM contacts.consents
WHERE phone_e164 = $1 AND channel = $2 AND purpose = $3
FOR UPDATE;

-- name: ListRecentConsentEvents :many
SELECT * FROM contacts.consent_events WHERE phone_e164 = $1
ORDER BY occurred_at DESC, id DESC
LIMIT sqlc.arg(max_rows);

-- ConsentForNumbers is CheckConsent's one query (data model §10): for
-- each distinct number, its consent for the channel and purpose (empty
-- when none) and whether it's suppressed on that channel or every one.
-- name: ConsentForNumbers :many
SELECT n.phone::text AS phone,
       coalesce(c.status, '')::text AS status,
       EXISTS (SELECT 1 FROM contacts.suppressions s
               WHERE s.tenant_id = sqlc.arg(tenant_id) AND s.phone_e164 = n.phone
                 AND (s.channel IS NULL OR s.channel = sqlc.arg(channel)::contacts.channel)) AS suppressed
FROM (SELECT DISTINCT unnest(sqlc.arg(phones)::text[]) AS phone) n
LEFT JOIN contacts.consents c
  ON c.tenant_id = sqlc.arg(tenant_id) AND c.phone_e164 = n.phone
 AND c.channel = sqlc.arg(channel)::contacts.channel AND c.purpose = sqlc.arg(purpose)::contacts.purpose;
