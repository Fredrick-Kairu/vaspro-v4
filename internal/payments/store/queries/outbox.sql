-- name: CountOutboxEvents :one
SELECT count(*) FROM payments.outbox_events WHERE event_type = $1 AND aggregate_id = $2;
