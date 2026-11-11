//go:build integration

package store_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestReversals_Constraints covers migration 20261106160000 (task 3.7):
// each status carries exactly the decision columns it should, a
// rejection is never by its maker, a pay-back is only for the payback
// kind, and paid_back always names its pay-back transaction.
func TestReversals_Constraints(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	var txn uuid.UUID
	if err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
		var err error
		_, txn, err = fundedServiceWallet(ctx, tx, tenant, 80000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	maker, checker := uuid.New(), uuid.New()

	insert := func(cols string, vals ...any) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error {
			args := append([]any{uuid.New(), tenant, txn, maker}, vals...)
			placeholders := ""
			for i := range vals {
				placeholders += ", $" + strconv.Itoa(5+i)
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO reversals (id, tenant_id, original_transaction_id, requested_by, amount_minor, reason, currency`+cols+`)
				VALUES ($1, $2, $3, $4, 100, 'duplicate payment', 'KES'`+placeholders+`)`, args...)
			return err
		})
	}

	ok := []struct {
		name string
		cols string
		vals []any
	}{
		{"requested", ", kind, status", []any{"payback", "requested"}},
		{"rejected by someone else, with a reason", ", kind, status, rejected_by, rejection_reason, decided_at",
			[]any{"payback", "rejected", checker, "not a duplicate", "2026-11-09 10:00Z"}},
		{"a pay-back pending", ", kind, status, approved_by, reversal_transaction_id, decided_at",
			[]any{"payback", "payback_pending", checker, txn, "2026-11-09 10:00Z"}},
		{"paid back", ", kind, status, approved_by, reversal_transaction_id, decided_at, payback_transaction_id",
			[]any{"payback", "paid_back", checker, txn, "2026-11-09 10:00Z", txn}},
		{"a chargeback posted", ", kind, status, approved_by, reversal_transaction_id, decided_at",
			[]any{"chargeback", "posted", checker, txn, "2026-11-09 10:00Z"}},
	}
	for _, tc := range ok {
		t.Run("allowed: "+tc.name, func(t *testing.T) {
			if err := insert(tc.cols, tc.vals...); err != nil {
				t.Fatal(err)
			}
		})
	}

	refused := []struct {
		name string
		cols string
		vals []any
		code string
	}{
		{"no kind", ", status", []any{"requested"}, "23502"},
		{"an unknown kind", ", kind, status", []any{"refund", "requested"}, "23514"},
		{"requested but already approved", ", kind, status, approved_by", []any{"payback", "requested", checker}, "23514"},
		{"rejected by its maker", ", kind, status, rejected_by, rejection_reason, decided_at",
			[]any{"payback", "rejected", maker, "no", "2026-11-09 10:00Z"}, "23514"},
		{"rejected without a reason", ", kind, status, rejected_by, decided_at",
			[]any{"payback", "rejected", checker, "2026-11-09 10:00Z"}, "23514"},
		{"rejected with an approver", ", kind, status, rejected_by, rejection_reason, approved_by, decided_at",
			[]any{"payback", "rejected", checker, "no", checker, "2026-11-09 10:00Z"}, "23514"},
		{"posted without its transaction", ", kind, status, approved_by, decided_at",
			[]any{"chargeback", "posted", checker, "2026-11-09 10:00Z"}, "23514"},
		{"posted without an approver", ", kind, status, reversal_transaction_id, decided_at",
			[]any{"chargeback", "posted", txn, "2026-11-09 10:00Z"}, "23514"},
		{"paid back without a pay-back transaction", ", kind, status, approved_by, reversal_transaction_id, decided_at",
			[]any{"payback", "paid_back", checker, txn, "2026-11-09 10:00Z"}, "23514"},
		{"a pay-back transaction on a pending one", ", kind, status, approved_by, reversal_transaction_id, decided_at, payback_transaction_id",
			[]any{"payback", "payback_pending", checker, txn, "2026-11-09 10:00Z", txn}, "23514"},
		{"a chargeback awaiting pay-back", ", kind, status, approved_by, reversal_transaction_id, decided_at",
			[]any{"chargeback", "payback_pending", checker, txn, "2026-11-09 10:00Z"}, "23514"},
		{"a pay-back reversal merely posted", ", kind, status, approved_by, reversal_transaction_id, decided_at",
			[]any{"payback", "posted", checker, txn, "2026-11-09 10:00Z"}, "23514"},
		{"negative pay-back attempts", ", kind, status, payback_attempts", []any{"payback", "requested", -1}, "23514"},
	}
	for _, tc := range refused {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			wantCode(t, insert(tc.cols, tc.vals...), tc.code)
		})
	}

	t.Run("refused: an unknown currency", func(t *testing.T) {
		err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO reversals (id, tenant_id, original_transaction_id, requested_by, amount_minor, reason, currency, kind, status)
				VALUES ($1, $2, $3, $4, 100, 'x', 'XXX', 'payback', 'requested')`, uuid.New(), tenant, txn, maker)
			return err
		})
		wantCode(t, err, "23503")
	})
}

// TestReversalReviewRole: ledger_reversal_review reads every client's
// reversals (the staff queue), only inside WithRoleTx, and writes none.
func TestReversalReviewRole(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	for range 2 {
		tenant := uuid.New()
		if err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
			_, txn, err := fundedServiceWallet(ctx, tx, tenant, 80000)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO reversals (id, tenant_id, original_transaction_id, amount_minor, reason, status, requested_by, kind, currency)
				VALUES ($1, $2, $3, 100, 'x', 'requested', $4, 'payback', 'KES')`, uuid.New(), tenant, txn, uuid.New())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	var seen int
	if err := db.app.WithRoleTx(ctx, "ledger_reversal_review", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		seen, err = count(ctx, tx, `SELECT count(*) FROM reversals`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("the review role sees %d reversals, want both clients' 2", seen)
	}
	outside, err := count(ctx, db.app, `SELECT count(*) FROM reversals`)
	if err != nil {
		t.Fatal(err)
	}
	if outside != 0 {
		t.Fatalf("outside WithRoleTx: %d reversals visible, want 0", outside)
	}
	err = db.app.WithRoleTx(ctx, "ledger_reversal_review", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE reversals SET reason = 'changed'`)
		return err
	})
	wantCode(t, err, "42501")
}
