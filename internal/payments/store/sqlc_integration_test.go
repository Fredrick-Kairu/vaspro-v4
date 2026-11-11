//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/store"
)

// TestSqlc_RoundTripUnderRLS: the generated queries work as the
// service would use them, in the tenant's transaction or as
// payments_system, and the ON CONFLICT queries report a repeat as
// pgx.ErrNoRows.
func TestSqlc_RoundTripUnderRLS(t *testing.T) {
	ctx := context.Background()
	db := newPaymentsDB(t)
	tenant := uuid.New()

	var pb uuid.UUID
	if err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := db.super.Exec(ctx, `INSERT INTO rails (code, country_code, kind, capabilities, status) VALUES ('mpesa_ke', 'KE', 'mobile_money', '{stk,c2b}', 'live')`); err != nil {
			return err
		}
		acct, err := q.CreateRailAccount(ctx, store.CreateRailAccountParams{
			ID: uuid.New(), RailCode: "mpesa_ke", AccountType: "paybill", Number: "600100",
			DisplayName: "VASPRO", Uses: []string{"vaspro_topups"}, Status: "pending_verification",
		})
		pb = acct.ID
		return err
	}); err != nil {
		t.Fatalf("payments_system creates VASPRO's paybill: %v", err)
	}

	var intentID uuid.UUID
	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		q := store.New(tx)
		in, err := q.CreatePaymentIntent(ctx, store.CreatePaymentIntentParams{
			ID: uuid.New(), TenantID: tenant, Purpose: store.PaymentsIntentPurposeWalletTopup, RailAccountID: pb,
			Method: "stk_push", AmountMinor: 150000, Currency: "KES",
			PayerPhoneE164: pgtype.Text{String: sandboxPhone, Valid: true}, AccountReference: "ABC23456",
			IdempotencyKey: "key-1", ExpiresAt: time.Now().Add(2 * time.Minute), CreatedAt: time.Now(),
		})
		if err != nil {
			return err
		}
		intentID = in.ID
		if in.Status != store.PaymentsIntentStatusCreated {
			t.Fatalf("status %q, want created", in.Status)
		}
		if _, err := q.CreateTopupAccount(ctx, store.CreateTopupAccountParams{ID: uuid.New(), TenantID: tenant, RailAccountID: pb, AccountNumber: "ABC23456"}); err != nil {
			return err
		}
		notice := store.InsertCollectionNoticeParams{
			ID: uuid.New(), RailAccountID: pb, RailReceipt: "NLJ7RT61SV", AmountMinor: 150000, Currency: "KES",
			PaidAt: time.Now(), MatchStatus: "matched", MatchedTenantID: pgtype.UUID{Bytes: tenant, Valid: true},
			PaymentIntentID: pgtype.UUID{Bytes: in.ID, Valid: true}, Raw: []byte(`{}`),
		}
		if _, err := q.InsertCollectionNotice(ctx, notice); err != nil {
			return err
		}
		notice.ID = uuid.New()
		if _, err := q.InsertCollectionNotice(ctx, notice); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a repeated receipt: %v, want pgx.ErrNoRows", err)
		}
		_, err = tx.Exec(ctx, `UPDATE payment_intents SET status = 'pending', rail_request_id = 'ws_CO_1' WHERE id = $1`, in.ID)
		return err
	}); err != nil {
		t.Fatalf("tenant round trip: %v", err)
	}

	if err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		q := store.New(tx)
		in, err := q.GetPaymentIntentByRailRequest(ctx, store.GetPaymentIntentByRailRequestParams{RailAccountID: pb, RailRequestID: pgtype.Text{String: "ws_CO_1", Valid: true}})
		if err != nil || in.ID != intentID {
			t.Fatalf("intent by request ID: %+v %v", in, err)
		}
		ta, err := q.GetTopupAccountByNumber(ctx, store.GetTopupAccountByNumberParams{RailAccountID: pb, AccountNumber: "ABC23456"})
		if err != nil || ta.TenantID != tenant {
			t.Fatalf("account by number: %+v %v", ta, err)
		}
		cb := store.InsertSTKCallbackParams{ID: uuid.New(), RailAccountID: pb, CheckoutRequestID: "ws_CO_1", ResultCode: "0", Raw: []byte(`{}`)}
		if _, err := q.InsertSTKCallback(ctx, cb); err != nil {
			return err
		}
		cb.ID = uuid.New()
		if _, err := q.InsertSTKCallback(ctx, cb); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a repeated callback: %v, want pgx.ErrNoRows", err)
		}
		got, err := q.GetSTKCallback(ctx, store.GetSTKCallbackParams{RailAccountID: pb, CheckoutRequestID: "ws_CO_1"})
		if err != nil || got.ResultCode != "0" {
			t.Fatalf("callback: %+v %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("payments_system round trip: %v", err)
	}
}
