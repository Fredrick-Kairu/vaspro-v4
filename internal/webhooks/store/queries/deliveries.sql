-- InsertDelivery returns no row (pgx.ErrNoRows) when the event already
-- has a delivery to this endpoint: a redelivered event is sent once.
-- name: InsertDelivery :one
INSERT INTO webhooks.deliveries (id, tenant_id, endpoint_id, event_id, event_type, occurred_at, body, next_attempt_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.arg(next_attempt_at)::timestamptz)
ON CONFLICT (endpoint_id, event_id) WHERE replay_of IS NULL DO NOTHING
RETURNING *;

-- CreateReplay copies a delivery's event and body to send them again.
-- name: CreateReplay :one
INSERT INTO webhooks.deliveries (id, tenant_id, endpoint_id, event_id, event_type, occurred_at, body,
  next_attempt_at, replay_of, replayed_by)
SELECT sqlc.arg(id), d.tenant_id, d.endpoint_id, d.event_id, d.event_type, d.occurred_at, d.body,
  sqlc.arg(next_attempt_at)::timestamptz, d.id, sqlc.arg(replayed_by)::uuid
FROM webhooks.deliveries d WHERE d.id = sqlc.arg(replay_of)
RETURNING *;

-- name: GetDelivery :one
SELECT * FROM webhooks.deliveries WHERE id = $1;

-- ClaimDueDeliveries leases up to max_rows due deliveries across
-- clients, skipping rows another worker holds. webhooks_system only.
-- name: ClaimDueDeliveries :many
UPDATE webhooks.deliveries SET leased_until = sqlc.arg(lease_until)::timestamptz
WHERE id IN (
  SELECT d.id FROM webhooks.deliveries d
  WHERE d.status = 'pending' AND d.next_attempt_at <= sqlc.arg(now)::timestamptz
    AND (d.leased_until IS NULL OR d.leased_until <= sqlc.arg(now)::timestamptz)
  ORDER BY d.next_attempt_at
  LIMIT sqlc.arg(max_rows)
  FOR UPDATE SKIP LOCKED)
RETURNING *;

-- RecordDeliveryResult moves a delivery after an attempt: succeeded,
-- dead, or due again. Only from the attempt count the worker claimed it
-- at, so two workers can't both record one attempt. webhooks_system.
-- name: RecordDeliveryResult :execrows
UPDATE webhooks.deliveries
SET status = sqlc.arg(status), attempts = sqlc.arg(attempts_prev)::int + 1,
  next_attempt_at = sqlc.narg(next_attempt_at)::timestamptz, completed_at = sqlc.narg(completed_at)::timestamptz,
  dead_reason = sqlc.narg(dead_reason), leased_until = NULL
WHERE id = sqlc.arg(id) AND status = 'pending' AND attempts = sqlc.arg(attempts_prev)::int;

-- EndDelivery makes a pending delivery dead without an attempt: its
-- endpoint was deleted or disabled. webhooks_system.
-- name: EndDelivery :execrows
UPDATE webhooks.deliveries
SET status = 'dead', dead_reason = sqlc.arg(dead_reason), next_attempt_at = NULL,
  completed_at = sqlc.arg(at)::timestamptz, leased_until = NULL
WHERE id = sqlc.arg(id) AND status = 'pending';

-- PostponeDelivery holds a delivery without counting an attempt: its
-- endpoint is waiting for verification. webhooks_system.
-- name: PostponeDelivery :execrows
UPDATE webhooks.deliveries
SET next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz, leased_until = NULL
WHERE id = sqlc.arg(id) AND status = 'pending';

-- KillPendingDeliveries ends every pending delivery to an endpoint that
-- was just disabled. webhooks_system.
-- name: KillPendingDeliveries :execrows
UPDATE webhooks.deliveries
SET status = 'dead', dead_reason = sqlc.arg(dead_reason), next_attempt_at = NULL,
  completed_at = sqlc.arg(at)::timestamptz, leased_until = NULL
WHERE endpoint_id = sqlc.arg(endpoint_id) AND status = 'pending';

-- ListDeliveries is the client's delivery log, newest first, with
-- optional filters and a (created_at, id) cursor: rows strictly after
-- the cursor's in that order.
-- name: ListDeliveries :many
SELECT * FROM webhooks.deliveries
WHERE (sqlc.narg(endpoint_id)::uuid IS NULL OR endpoint_id = sqlc.narg(endpoint_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(event_type)::text IS NULL OR event_type = sqlc.narg(event_type)::text)
  AND (sqlc.narg(event_id)::uuid IS NULL OR event_id = sqlc.narg(event_id)::uuid)
  AND (sqlc.narg(before_created_at)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(before_created_at)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(max_rows);
