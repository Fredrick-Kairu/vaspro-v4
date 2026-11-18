-- name: AddSubscription :exec
INSERT INTO webhooks.subscriptions (tenant_id, endpoint_id, event_type)
VALUES ($1, $2, $3)
ON CONFLICT (endpoint_id, event_type) DO NOTHING;

-- name: ListSubscriptions :many
SELECT * FROM webhooks.subscriptions WHERE endpoint_id = $1 ORDER BY event_type;

-- name: DeleteSubscriptions :exec
DELETE FROM webhooks.subscriptions WHERE endpoint_id = $1;
