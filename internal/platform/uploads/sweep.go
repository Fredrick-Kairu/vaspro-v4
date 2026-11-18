package uploads

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Due is an upload whose files are past delete_after: its ID and every
// key it may have (quarantine, file, error report).
type Due struct {
	ID   uuid.UUID
	Keys []string
}

// SweepTarget is a service's upload table, as the sweeper sees it.
type SweepTarget interface {
	// Due lists at most limit uploads with delete_after <= now and no
	// deleted_at. A service lists only uploads whose clock has started
	// (a final status, or the creation default), so nothing still
	// processing is due.
	Due(ctx context.Context, now time.Time, limit int) ([]Due, error)
	// MarkDeleted sets deleted_at; the row and its results stay.
	MarkDeleted(ctx context.Context, id uuid.UUID, at time.Time) error
}

// SweepResult counts one pass.
type SweepResult struct {
	Deleted int // uploads whose files are gone and marked
	Failed  int // uploads left for the next pass
}

// Sweep deletes the files of every due upload, then marks it deleted.
// An upload is marked only when all its deletes succeeded (a missing
// object counts as deleted), so a failure is retried next pass and a
// second pass changes nothing. Data model §8, flow step 6.
func (u *Uploads) Sweep(ctx context.Context, target SweepTarget, limit int) (SweepResult, error) {
	now := u.clock.Now()
	due, err := target.Due(ctx, now, limit)
	if err != nil {
		return SweepResult{}, fmt.Errorf("uploads: listing due uploads: %w", err)
	}
	var res SweepResult
	for _, d := range due {
		if u.deleteAll(ctx, d.Keys) && target.MarkDeleted(ctx, d.ID, now) == nil {
			res.Deleted++
			continue
		}
		res.Failed++
	}
	return res, nil
}

func (u *Uploads) deleteAll(ctx context.Context, keys []string) bool {
	ok := true
	for _, k := range keys {
		if err := u.store.Delete(ctx, k); err != nil {
			ok = false
		}
	}
	return ok
}
