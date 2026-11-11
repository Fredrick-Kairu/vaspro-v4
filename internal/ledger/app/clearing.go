package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// MaxClearingPage is the most lines one ListClearingEntries page holds.
const MaxClearingPage = 1000

// ClearingEntry is one line on a rail's provider clearing account.
type ClearingEntry struct {
	EntryID       int64
	TransactionID uuid.UUID
	Direction     string
	AmountMinor   int64
	ReferenceType string
	ReferenceID   *uuid.UUID
	PostedAt      time.Time
}

// ClearingEntries lists a rail's provider clearing lines posted in
// [from, to), in entry order after afterEntryID, for payments' daily
// reconciliation (task 4.6, OQ-22). It reads as ledger_audit, which
// sees every client's postings, so a client top-up's line is listed
// with VASPRO's own. next is the last entry's ID when the page is full,
// else 0.
func (s *Service) ClearingEntries(ctx context.Context, rail, currency string, from, to time.Time, afterEntryID int64, limit int) ([]ClearingEntry, int64, error) {
	switch {
	case rail == "" || currency == "":
		return nil, 0, fmt.Errorf("%w: a rail and a currency are required", ErrValidation)
	case !from.Before(to):
		return nil, 0, fmt.Errorf("%w: the window must end after it starts", ErrValidation)
	case limit < 0 || limit > MaxClearingPage:
		return nil, 0, fmt.Errorf("%w: a page holds 1 to %d lines", ErrValidation, MaxClearingPage)
	}
	if limit == 0 {
		limit = MaxClearingPage
	}
	var rows []store.ListClearingEntriesRow
	err := s.pool.WithRoleTx(ctx, auditRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListClearingEntries(ctx, store.ListClearingEntriesParams{
			Rail: pgtype.Text{String: rail, Valid: true}, Currency: currency, PostedFrom: from, PostedTo: to,
			AfterEntryID: afterEntryID, MaxRows: int32(limit), //nolint:gosec // limit is 1..1000
		})
		return err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("app: listing clearing entries: %w", err)
	}
	out := make([]ClearingEntry, len(rows))
	for i, r := range rows {
		out[i] = ClearingEntry{
			EntryID: r.EntryID, TransactionID: r.TransactionID, Direction: r.Direction, AmountMinor: r.AmountMinor,
			ReferenceType: r.ReferenceType, PostedAt: r.PostedAt,
		}
		if r.ReferenceID.Valid {
			id := uuid.UUID(r.ReferenceID.Bytes)
			out[i].ReferenceID = &id
		}
	}
	var next int64
	if len(out) == limit {
		next = out[len(out)-1].EntryID
	}
	return out, next, nil
}
