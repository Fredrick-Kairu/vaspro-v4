//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

func numeric(t *testing.T, s string) pgtype.Numeric {
	t.Helper()
	var n pgtype.Numeric
	if err := n.Scan(s); err != nil {
		t.Fatalf("numeric %q: %v", s, err)
	}
	return n
}

func numericString(t *testing.T, n pgtype.Numeric) string {
	t.Helper()
	v, err := n.Value()
	if err != nil {
		t.Fatal(err)
	}
	return v.(string)
}

// TestSqlc_RoundTripUnderRLS: the generated queries work through
// ledger_app's grants and a tenant's RLS, and numeric(18,6) keeps a
// fraction of a cent exactly (no float anywhere).
func TestSqlc_RoundTripUnderRLS(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	tenantID := pgtype.UUID{Bytes: tenant, Valid: true}
	revenue := mustAccount(t, db.super, account{typ: "revenue", service: ptr("sms")})

	err := db.app.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		recv, err := q.CreateAccount(ctx, store.CreateAccountParams{
			ID: uuid.New(), TenantID: tenantID, Type: store.LedgerAccountTypeReceivable, Currency: "KES",
		})
		if err != nil {
			return err
		}
		txn, err := q.CreateTransaction(ctx, store.CreateTransactionParams{
			ID: uuid.New(), TenantID: tenantID, Type: store.LedgerTxnTypeAdjustment,
			IdempotencyKey: "k1", Description: "round trip",
		})
		if err != nil {
			return err
		}
		for _, p := range []store.CreateEntryParams{
			{TransactionID: txn.ID, AccountID: recv.ID, Direction: "D", AmountMinor: 30400, Currency: "KES"},
			{TransactionID: txn.ID, AccountID: revenue, Direction: "C", AmountMinor: 30400, Currency: "KES"},
		} {
			if _, err := q.CreateEntry(ctx, p); err != nil {
				return err
			}
		}
		lines, err := q.ListEntriesByTransaction(ctx, txn.ID)
		if err != nil {
			return err
		}
		if len(lines) != 2 {
			t.Errorf("ListEntriesByTransaction = %d lines, want 2", len(lines))
		}

		price, err := q.CreateTokenPrice(ctx, store.CreateTokenPriceParams{
			ID: uuid.New(), TenantID: tenantID, ServiceCode: "sms", Currency: "KES",
			MinTokens: numeric(t, "0"), MinorPerToken: numeric(t, "80.123456"),
			Status: "pending_approval", EffectiveFrom: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), CreatedBy: uuid.New(),
		})
		if err != nil {
			return err
		}
		got, err := q.GetTokenPrice(ctx, price.ID)
		if err != nil {
			return err
		}
		if s := numericString(t, got.MinorPerToken); s != "80.123456" {
			t.Errorf("minor_per_token = %s, want 80.123456", s)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
