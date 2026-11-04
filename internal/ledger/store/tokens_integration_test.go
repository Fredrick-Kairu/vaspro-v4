//go:build integration

package store_test

import (
	"context"
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
	price := listPrice(t, db, "0")
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
			capturedMinor := any(nil)
			if status == "captured" {
				capturedMinor = 1
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, tokens, status, expires_at, captured_minor, captured_tokens)
				VALUES ($1, $2, $3, 'campaign', $4, 100, $5, $6, now(), $7, $8)`,
				uuid.New(), tenant, wallet, uuid.New(), tokens, status, capturedMinor, captured)
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
