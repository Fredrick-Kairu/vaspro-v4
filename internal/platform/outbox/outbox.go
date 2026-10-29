// Package outbox implements the transactional outbox (data model §1):
// Write persists an event in the same transaction as the business change
// it records, and Relay later ships every unpublished row to the bus.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
)

// Write inserts a row for env into outbox_events inside tx. Call this in
// the same transaction as the business change env records, never after
// it commits.
func Write(ctx context.Context, tx pgx.Tx, aggregate string, aggregateID uuid.UUID, env events.Envelope) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("outbox: marshaling envelope: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (id, tenant_id, aggregate, aggregate_id, event_type, payload, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		env.ID, env.TenantID, aggregate, aggregateID, env.Type, payload, env.OccurredAt,
	)
	if err != nil {
		return fmt.Errorf("outbox: writing row: %w", err)
	}
	return nil
}

// Publisher is the narrow interface Relay needs to ship a message —
// platform/bus (task 0.9) will satisfy it; nothing here depends on NATS.
type Publisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// Relay publishes outbox_events rows that haven't been published yet.
type Relay struct {
	pool       *db.Pool
	pub        Publisher
	subjectFor func(eventType string) string
}

// NewRelay returns a Relay that publishes through pub, mapping each
// row's event_type to a bus subject via subjectFor.
func NewRelay(pool *db.Pool, pub Publisher, subjectFor func(eventType string) string) *Relay {
	return &Relay{pool: pool, pub: pub, subjectFor: subjectFor}
}

// RelayOnce publishes every unpublished row, oldest first, marking each
// published immediately after a successful Publish. A row whose Publish
// fails is left unpublished for a later pass to retry — at-least-once
// delivery, which is why consumers apply events through platform/inbox.
// Returns how many rows were relayed; a non-nil error means at least one
// row failed (joined, if more than one did), but rows before it in the
// batch may still have succeeded.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, event_type, payload
		FROM outbox_events
		WHERE published_at IS NULL
		ORDER BY occurred_at`)
	if err != nil {
		return 0, fmt.Errorf("outbox: querying unpublished rows: %w", err)
	}

	type pending struct {
		id        uuid.UUID
		eventType string
		payload   []byte
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.eventType, &p.payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("outbox: scanning row: %w", err)
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("outbox: reading rows: %w", err)
	}
	rows.Close()

	var relayed int
	var errs []error
	for _, p := range batch {
		if err := r.pub.Publish(ctx, r.subjectFor(p.eventType), p.payload); err != nil {
			errs = append(errs, fmt.Errorf("outbox: publishing %s: %w", p.id, err))
			continue
		}
		if _, err := r.pool.Exec(ctx, "UPDATE outbox_events SET published_at = now() WHERE id = $1", p.id); err != nil {
			errs = append(errs, fmt.Errorf("outbox: marking %s published: %w", p.id, err))
			continue
		}
		relayed++
	}
	return relayed, errors.Join(errs...)
}
