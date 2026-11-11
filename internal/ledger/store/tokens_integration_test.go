//go:build integration

package store_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// tokenWallet makes, as the superuser, a tenant's SMS wallet holding one
// lot of tokens (bought for 80 each, posted from a receivable).
func tokenWallet(t *testing.T, db ledgerDB, tokens string) (tenant, wallet, lot uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant, lot = uuid.New(), uuid.New()
	// Each call's own tier, so a second wallet's list price doesn't
	// overlap the first's (the EXCLUDE refuses that, rightly).
	price := listPrice(t, db, strconv.FormatUint(uint64(uuid.New().ID()), 10))
	if err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
		var txn uuid.UUID
		var err error
		wallet, txn, err = fundedServiceWallet(ctx, tx, tenant, 80000)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO token_lots (id, tenant_id, account_id, transaction_id, tokens_bought, tokens_remaining, minor_per_token, token_price_id, purchased_at)
			VALUES ($1, $2, $3, $4, $5, $5, 80, $6, now())`, lot, tenant, wallet, txn, tokens, price)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tenant, wallet, lot
}

// TestWalletGuard_HeldTokens: migration 20261106120000 extends 3.2's
// wallet guard: a service wallet can't hold more tokens than its open
// lots have left, whether held_tokens rises or a lot is drawn down
// under it. Checked at commit.
func TestWalletGuard_HeldTokens(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant, wallet, lot := tokenWallet(t, db, "1000")

	t.Run("holding more than is left fails at commit", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		if _, err := tx.Exec(ctx, `UPDATE balances SET held_tokens = 1000.000001 WHERE account_id = $1`, wallet); err != nil {
			t.Fatalf("the update itself should pass: %v", err)
		}
		wantErr(t, tx.Commit(ctx), "23514", "would hold")
	})

	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE balances SET held_tokens = 600 WHERE account_id = $1`, wallet)
		return err
	}); err != nil {
		t.Fatalf("holding 600 of 1000: %v", err)
	}

	t.Run("drawing a lot down under the held tokens fails at commit", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		if _, err := tx.Exec(ctx, `UPDATE token_lots SET tokens_remaining = 599 WHERE id = $1`, lot); err != nil {
			t.Fatalf("the update itself should pass: %v", err)
		}
		wantErr(t, tx.Commit(ctx), "23514", "would hold")
	})

	t.Run("drawing down to exactly what is held commits", func(t *testing.T) {
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE token_lots SET tokens_remaining = 600 WHERE id = $1`, lot)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("held_tokens can't go negative", func(t *testing.T) {
		err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE balances SET held_tokens = -1 WHERE account_id = $1`, wallet)
			return err
		})
		wantCode(t, err, "23514")
	})
}

func TestTokens_Constraints(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant, wallet, lot := tokenWallet(t, db, "1000")
	var txn uuid.UUID
	if err := db.super.QueryRow(ctx, `SELECT transaction_id FROM token_lots WHERE id = $1`, lot).Scan(&txn); err != nil {
		t.Fatal(err)
	}
	price := listPrice(t, db, "10")

	insertHold := func(status string, tokens, captured any) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error {
			capturedMinor, capturedEvents := any(nil), any(nil)
			if status == "captured" {
				capturedMinor = 1
				if tokens != nil {
					capturedEvents = 1 // 3.5's CHECK: a captured token hold records its events
				}
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, tokens, status, expires_at, captured_minor, captured_tokens, captured_events)
				VALUES ($1, $2, $3, 'campaign', $4, 100, $5, $6, now(), $7, $8, $9)`,
				uuid.New(), tenant, wallet, uuid.New(), tokens, status, capturedMinor, captured, capturedEvents)
			return err
		})
	}
	insertLot := func(bought, remaining, price string, priceID uuid.UUID) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO token_lots (id, tenant_id, account_id, transaction_id, tokens_bought, tokens_remaining, minor_per_token, token_price_id, purchased_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`, uuid.New(), tenant, wallet, txn, bought, remaining, price, priceID)
			return err
		})
	}

	for _, tc := range []struct {
		name string
		err  error
		ok   bool
	}{
		{"a captured token hold with captured tokens", insertHold("captured", "400", "380"), true},
		{"an active token hold", insertHold("active", "400", nil), true},
		{"a captured money hold, no tokens", insertHold("captured", nil, nil), true},
		{"a captured token hold without captured tokens", insertHold("captured", "400", nil), false},
		{"an active token hold with captured tokens", insertHold("active", "400", "1"), false},
		{"captured tokens beyond the hold", insertHold("captured", "400", "400.000001"), false},
		{"zero captured tokens", insertHold("captured", "400", "0"), false},
		{"a hold of zero tokens", insertHold("active", "0", nil), false},
		{"a lot", insertLot("10", "10", "80", price), true},
		{"a lot of zero tokens", insertLot("0", "0", "80", price), false},
		{"a lot with more left than bought", insertLot("10", "10.000001", "80", price), false},
		{"a lot at a zero price", insertLot("10", "10", "0", price), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ok && tc.err != nil {
				t.Fatal(tc.err)
			}
			if !tc.ok {
				wantCode(t, tc.err, "23514")
			}
		})
	}
}

// TestPriceReviewRole: ledger_price_review (migration 20261106130000)
// reads every client's token prices, only inside WithRoleTx, and can't
// write them.
func TestPriceReviewRole(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	for _, tenant := range []*uuid.UUID{ptr(uuid.New()), ptr(uuid.New()), nil} {
		if _, err := db.super.Exec(ctx, `
			INSERT INTO token_prices (id, tenant_id, service_code, currency, minor_per_token, status, effective_from, created_by)
			VALUES ($1, $2, 'sms', 'KES', 80, 'pending_approval', now(), $3)`, uuid.New(), tenant, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}

	var seen int
	if err := db.app.WithRoleTx(ctx, "ledger_price_review", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		seen, err = count(ctx, tx, `SELECT count(*) FROM token_prices`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 3 {
		t.Fatalf("the review role sees %d prices, want all 3", seen)
	}

	outside, err := count(ctx, db.app, `SELECT count(*) FROM token_prices WHERE tenant_id IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	if outside != 0 {
		t.Fatalf("outside WithRoleTx: %d client prices visible, want 0", outside)
	}

	err = db.app.WithRoleTx(ctx, "ledger_price_review", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE token_prices SET status = 'withdrawn'`)
		return err
	})
	wantCode(t, err, "42501")
}

// TestSettlement_ConstraintsAndRole covers migration 20261106140000:
// captured_events goes with a captured token hold; a usage batch's
// window runs forward and holds tokens; ledger_settlement sees captured,
// unsettled token holds across clients (and, since 3.6, active holds
// for the expiry job), never settled ones, only inside WithRoleTx.
func TestSettlement_ConstraintsAndRole(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant, wallet, _ := tokenWallet(t, db, "1000")

	hold := func(status string, tokens, capturedTokens, events, settled any) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error {
			capturedMinor := any(nil)
			if status == "captured" {
				capturedMinor = 1
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, tokens, status,
				                   expires_at, captured_minor, captured_tokens, captured_events, settled_at)
				VALUES ($1, $2, $3, 'campaign', $4, 100, $5, $6, now(), $7, $8, $9, $10)`,
				uuid.New(), tenant, wallet, uuid.New(), tokens, status, capturedMinor, capturedTokens, events, settled)
			return err
		})
	}
	batch := func(start, end string, tokens string) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO usage_batches (id, tenant_id, account_id, window_start, window_end, events, tokens)
				VALUES ($1, $2, $3, $4, $5, 0, $6)`, uuid.New(), tenant, wallet, start, end, tokens)
			return err
		})
	}

	for _, tc := range []struct {
		name string
		err  error
		ok   bool
	}{
		{"a captured token hold with its events", hold("captured", "10", "9", 9, nil), true},
		{"a captured token hold without events", hold("captured", "10", "9", nil, nil), false},
		{"an active token hold with events", hold("active", "10", nil, 1, nil), false},
		{"zero events", hold("captured", "10", "9", 0, nil), false},
		{"a window that runs backwards", batch("2026-11-04 09:05Z", "2026-11-04 09:00Z", "1"), false},
		{"a batch of no tokens", batch("2026-11-04 09:00Z", "2026-11-04 09:05Z", "0"), false},
		{"a batch", batch("2026-11-04 09:00Z", "2026-11-04 09:05Z", "1"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ok && tc.err != nil {
				t.Fatal(tc.err)
			}
			if !tc.ok {
				wantCode(t, tc.err, "23514")
			}
		})
	}

	// One more client's unsettled capture, a settled one and an active one.
	other, otherWallet, _ := tokenWallet(t, db, "1000")
	if err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, tokens, status, expires_at, captured_minor, captured_tokens, captured_events)
			VALUES ($1, $2, $3, 'campaign', $4, 100, 5, 'captured', now(), 1, 5, 5)`, uuid.New(), other, otherWallet, uuid.New())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := hold("captured", "10", "9", 9, "2026-11-04 09:05Z"); err != nil {
		t.Fatal(err)
	}
	if err := hold("active", "10", nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	var seen int
	if err := db.app.WithRoleTx(ctx, "ledger_settlement", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		seen, err = count(ctx, tx, `SELECT count(*) FROM holds`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The 2 captured and unsettled (one per client), and since 3.6 the
	// active one too (the expiry job's policy); never the settled one.
	if seen != 3 {
		t.Fatalf("the jobs' role sees %d holds, want the 2 captured and unsettled plus the active one", seen)
	}
	outside, err := count(ctx, db.app, `SELECT count(*) FROM holds`)
	if err != nil {
		t.Fatal(err)
	}
	if outside != 0 {
		t.Fatalf("outside WithRoleTx: %d holds visible, want 0", outside)
	}
	err = db.app.WithRoleTx(ctx, "ledger_settlement", func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE holds SET settled_at = now()`)
		return err
	})
	wantCode(t, err, "42501")
}

// TestExpiryAndAutoBuy_Constraints covers migration 20261106150000: an
// auto-buy rule's level is 0 or more and it buys a positive amount; the
// jobs' role (ledger_settlement) also sees active holds, so the expiry
// job can find them, and still no settled ones.
func TestExpiryAndAutoBuy_Constraints(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant, wallet, _ := tokenWallet(t, db, "1000")

	rule := func(below, buy string) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO auto_buy_rules (id, tenant_id, service_code, currency, below_tokens, buy_tokens)
				VALUES ($1, $2, $3, 'KES', $4, $5)`, uuid.New(), tenant, "s"+strconv.Itoa(int(uuid.New().ID()%100000)), below, buy)
			return err
		})
	}
	if err := rule("0", "1"); err != nil {
		t.Fatalf("a rule of level 0: %v", err)
	}
	wantCode(t, rule("-1", "1"), "23514")
	wantCode(t, rule("1", "0"), "23514")

	insert := func(status string, settled any) {
		t.Helper()
		if err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, status, expires_at, settled_at)
				VALUES ($1, $2, $3, 'payout', $4, 1, $5, '2026-11-04 09:00Z', $6)`, uuid.New(), tenant, wallet, uuid.New(), status, settled)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert("active", nil)
	insert("released", "2026-11-04 09:01Z")
	insert("expired", "2026-11-04 09:02Z")

	var seen int
	if err := db.app.WithRoleTx(ctx, "ledger_settlement", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		seen, err = count(ctx, tx, `SELECT count(*) FROM holds`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("the jobs' role sees %d holds, want only the active one", seen)
	}
}
