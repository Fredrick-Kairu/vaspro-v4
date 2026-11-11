//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestRebuildRuns_Constraints covers migration 20261106170000 (task
// 3.8): one row per date, and each status carries what it should.
func TestRebuildRuns_Constraints(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	insert := func(date, status string, finished any, accounts, txns, diffs any) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO rebuild_runs (id, run_date, status, started_at, finished_at, accounts_checked, transactions_checked, differences)
				VALUES ($1, $2, $3, now(), $4, $5, $6, $7)`, uuid.New(), date, status, finished, accounts, txns, diffs)
			return err
		})
	}
	for name, err := range map[string]error{
		"running":       insert("2026-11-01", "running", nil, nil, nil, nil),
		"passed with 0": insert("2026-11-02", "passed", "2026-11-02 21:31Z", 10, 5, 0),
		"failed with 3": insert("2026-11-03", "failed", "2026-11-03 21:31Z", 10, 5, 3),
	} {
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"a second row for a date":    {insert("2026-11-01", "running", nil, nil, nil, nil), "23505"},
		"an unknown status":          {insert("2026-11-04", "done", nil, nil, nil, nil), "23514"},
		"running but finished":       {insert("2026-11-05", "running", "2026-11-05 21:31Z", nil, nil, nil), "23514"},
		"passed but not finished":    {insert("2026-11-06", "passed", nil, 10, 5, 0), "23514"},
		"passed without counts":      {insert("2026-11-07", "passed", "2026-11-07 21:31Z", nil, nil, 0), "23514"},
		"passed with differences":    {insert("2026-11-08", "passed", "2026-11-08 21:31Z", 10, 5, 1), "23514"},
		"failed without differences": {insert("2026-11-09", "failed", "2026-11-09 21:31Z", 10, 5, 0), "23514"},
		"negative counts":            {insert("2026-11-10", "passed", "2026-11-10 21:31Z", -1, 5, 0), "23514"},
	} {
		t.Run(name, func(t *testing.T) { wantCode(t, tc.err, tc.code) })
	}
}

// TestAuditRole: ledger_audit reads every client's journal and copies,
// only inside WithRoleTx, and writes nothing.
func TestAuditRole(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	for range 2 {
		tokenWallet(t, db, "1000") // a client's posting, wallet and lot
	}
	tables := []string{"accounts", "balances", "transactions", "entries", "token_lots"}
	for _, table := range tables {
		var inside, outside int
		if err := db.app.WithRoleTx(ctx, "ledger_audit", func(ctx context.Context, tx pgx.Tx) error {
			var err error
			inside, err = count(ctx, tx, `SELECT count(*) FROM `+table+` WHERE `+clientRows(table))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		outside, err := count(ctx, db.app, `SELECT count(*) FROM `+table+` WHERE `+clientRows(table))
		if err != nil {
			t.Fatal(err)
		}
		if inside < 2 || outside != 0 {
			t.Fatalf("%s: %d client rows inside WithRoleTx (want both clients'), %d outside (want 0)", table, inside, outside)
		}
	}
	err := db.app.WithRoleTx(ctx, "ledger_audit", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'frozen'`)
		return err
	})
	wantCode(t, err, "42501")
	err = db.app.WithRoleTx(ctx, "ledger_audit", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO rebuild_runs (id, run_date, status, started_at) VALUES ($1, '2026-11-01', 'running', now())`, uuid.New())
		return err
	})
	wantCode(t, err, "42501")
}

// clientRows narrows a table to client-owned rows.
func clientRows(table string) string {
	switch table {
	case "balances":
		return "account_id IN (SELECT id FROM accounts WHERE tenant_id IS NOT NULL)"
	case "entries":
		return "transaction_id IN (SELECT id FROM transactions WHERE tenant_id IS NOT NULL)"
	default:
		return "tenant_id IS NOT NULL"
	}
}
