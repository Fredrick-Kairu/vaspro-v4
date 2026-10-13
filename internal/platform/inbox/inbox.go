// Package inbox guards against applying the same event twice (data model
// §1's processed_events): outbox delivery is at-least-once, so every
// consumer must check MarkProcessed before doing any work for an event.
package inbox

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MarkProcessed records eventID as processed inside tx — call this in
// the same transaction as the work the event causes, before doing that
// work. alreadyProcessed is true if eventID was already recorded (by an
// earlier delivery of the same event); the caller should skip applying
// it again.
func MarkProcessed(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (alreadyProcessed bool, err error) {
	tag, err := tx.Exec(ctx,
		"INSERT INTO processed_events (event_id) VALUES ($1) ON CONFLICT (event_id) DO NOTHING",
		eventID,
	)
	if err != nil {
		return false, fmt.Errorf("inbox: marking %s processed: %w", eventID, err)
	}
	return tag.RowsAffected() == 0, nil
}
