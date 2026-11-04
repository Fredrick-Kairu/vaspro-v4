//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestHolds_CapturedMinor covers migration 20261106110000's CHECKs: a
// captured amount exactly when captured, positive and within the hold.
func TestHolds_CapturedMinor(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	var wallet uuid.UUID
	if err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
		var err error
		wallet, _, err = fundedServiceWallet(ctx, tx, tenant, 80000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	insert := func(status string, captured any) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, status, expires_at, captured_minor)
				VALUES ($1, $2, $3, 'campaign', $4, 32000, $5, now(), $6)`, uuid.New(), tenant, wallet, uuid.New(), status, captured)
			return err
		})
	}

	for _, tc := range []struct {
		name     string
		status   string
		captured any
		ok       bool
	}{
		{"captured with an amount", "captured", 30400, true},
		{"captured with the whole amount", "captured", 32000, true},
		{"active with no amount", "active", nil, true},
		{"captured with no amount", "captured", nil, false},
		{"active with an amount", "active", 100, false},
		{"captured with more than the hold", "captured", 32001, false},
		{"captured with zero", "captured", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := insert(tc.status, tc.captured)
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok {
				wantCode(t, err, "23514")
			}
		})
	}
}

// TestLedgerSystem_ReadsLinesOnVASPROAccountsOnly: ledger_system sums
// VASPRO's balances from the journal (ADR 0010), so it reads every line
// on VASPRO's accounts, client postings' included, and still no client
// account's lines.
func TestLedgerSystem_ReadsLinesOnVASPROAccountsOnly(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	revenue := mustAccount(t, db.super, account{typ: "revenue", service: ptr("sms")})
	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		recv, err := insertAccount(ctx, tx, account{tenant: &tenant, typ: "receivable"})
		if err != nil {
			return err
		}
		_, err = post(ctx, tx, &tenant, debit(recv, 700), credit(revenue, 700))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var onRevenue, total int
	if err := asSystem(ctx, db.app, func(tx pgx.Tx) error {
		var err error
		if onRevenue, err = count(ctx, tx, `SELECT count(*) FROM entries WHERE account_id = $1`, revenue); err != nil {
			return err
		}
		total, err = count(ctx, tx, `SELECT count(*) FROM entries`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if onRevenue != 1 || total != 1 {
		t.Fatalf("ledger_system sees %d lines (%d on revenue), want only the revenue line", total, onRevenue)
	}

	outside, err := count(ctx, db.app, `SELECT count(*) FROM entries`)
	if err != nil {
		t.Fatal(err)
	}
	if outside != 0 {
		t.Fatalf("outside WithRoleTx: %d lines visible, want 0", outside)
	}
}
