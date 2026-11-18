-- name: CreateDeliveryAttempt :one
INSERT INTO webhooks.delivery_attempts (id, tenant_id, delivery_id, attempt, started_at, duration_ms,
  request_headers, outcome, response_status, response_headers, response_body, response_truncated, error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: ListDeliveryAttempts :many
SELECT * FROM webhooks.delivery_attempts WHERE delivery_id = $1 ORDER BY attempt;
