-- name: CreateEndpoint :one
INSERT INTO webhooks.endpoints (id, tenant_id, url, description, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetEndpoint :one
SELECT * FROM webhooks.endpoints WHERE id = $1 AND deleted_at IS NULL;

-- name: ListEndpoints :many
SELECT * FROM webhooks.endpoints WHERE deleted_at IS NULL ORDER BY created_at, id;

-- name: GetEndpointForUpdate :one
SELECT * FROM webhooks.endpoints WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: CountEndpoints :one
SELECT count(*) FROM webhooks.endpoints WHERE deleted_at IS NULL;

-- MarkEndpointVerified activates an endpoint whose challenge was just
-- answered: a pending one, a disabled one (re-enabling needs the
-- challenge too), or an active one checked again. Only at the version
-- the challenge was sent for: a URL changed meanwhile leaves it alone.
-- name: MarkEndpointVerified :execrows
UPDATE webhooks.endpoints
SET status = 'active', verified_at = sqlc.arg(at)::timestamptz, failing_since = NULL,
  disabled_at = NULL, disabled_reason = NULL, updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND deleted_at IS NULL;

-- UpdateEndpointURL points an endpoint somewhere new, which must answer
-- the challenge before it receives anything.
-- name: UpdateEndpointURL :one
UPDATE webhooks.endpoints
SET url = sqlc.arg(url), status = 'pending_verification', verified_at = NULL, failing_since = NULL,
  disabled_at = NULL, disabled_reason = NULL, updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING *;

-- name: UpdateEndpointDescription :one
UPDATE webhooks.endpoints
SET description = sqlc.narg(description), updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING *;

-- DeleteEndpoint keeps the row, so the delivery log still names it.
-- name: DeleteEndpoint :execrows
UPDATE webhooks.endpoints
SET deleted_at = sqlc.arg(at)::timestamptz, updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- ListEndpointsForEvent is the fan-out: the client's active endpoints
-- subscribed to an event type, or to every type. Run in the event's
-- tenant transaction.
-- name: ListEndpointsForEvent :many
SELECT e.* FROM webhooks.endpoints e
WHERE e.status = 'active' AND e.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM webhooks.subscriptions s
              WHERE s.endpoint_id = e.id AND s.event_type IN (sqlc.arg(event_type)::text, '*'))
ORDER BY e.id;

-- GetEndpointForDelivery reads a delivery's endpoint, deleted or not,
-- across clients. webhooks_system.
-- name: GetEndpointForDelivery :one
SELECT * FROM webhooks.endpoints WHERE id = $1;

-- MarkEndpointFailing records a failed attempt: failing_since is the
-- first since the last success. webhooks_system.
-- name: MarkEndpointFailing :one
UPDATE webhooks.endpoints
SET failing_since = coalesce(failing_since, sqlc.arg(at)::timestamptz)
WHERE id = sqlc.arg(id)
RETURNING failing_since;

-- name: ClearEndpointFailing :exec
UPDATE webhooks.endpoints SET failing_since = NULL WHERE id = $1 AND failing_since IS NOT NULL;

-- DisableFailingEndpoint disables an active endpoint failing since
-- before the cutoff. No row: someone else already did, or it recovered.
-- name: DisableFailingEndpoint :one
UPDATE webhooks.endpoints
SET status = 'disabled', disabled_at = sqlc.arg(at)::timestamptz, disabled_reason = 'failing',
  updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id) AND status = 'active' AND failing_since <= sqlc.arg(cutoff)::timestamptz
RETURNING *;

-- DisableEndpointByClient turns an endpoint off at the client's request
-- (re-enabled by passing the challenge). Already disabled, it's left as
-- it is, reason included.
-- name: DisableEndpointByClient :execrows
UPDATE webhooks.endpoints
SET status = 'disabled', disabled_at = sqlc.arg(at)::timestamptz, disabled_reason = 'client',
  updated_at = sqlc.arg(at)::timestamptz, version = version + 1
WHERE id = sqlc.arg(id) AND status <> 'disabled' AND deleted_at IS NULL;

-- LockTenantEndpoints serialises a client's endpoint creation until the
-- transaction ends, so two at once can't both pass the cap.
-- name: LockTenantEndpoints :exec
SELECT pg_advisory_xact_lock(hashtextextended('webhooks.endpoints:' || sqlc.arg(tenant_id)::uuid::text, 0));
