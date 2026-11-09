//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The constraint tests run as the superuser, which skips RLS but not
// CHECKs, keys or triggers. Each case runs in its own transaction.

func TestAccounts_Constraints(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()

	mustAccount(t, db.super, account{typ: "suspense"})
	mustAccount(t, db.super, account{tenant: &tenant, typ: "main_wallet"})
	mustAccount(t, db.super, account{typ: "provider_clearing", rail: ptr("mpesa_ke")})

	cases := []struct {
		name string
		a    account
		code string
	}{
		{"a second KES suspense account (NULLS NOT DISTINCT)", account{typ: "suspense"}, "23505"},
		{"a second KES main wallet for one client", account{tenant: &tenant, typ: "main_wallet"}, "23505"},
		{"a second M-Pesa KE clearing account", account{typ: "provider_clearing", rail: ptr("mpesa_ke")}, "23505"},
		{"a client wallet without a tenant", account{typ: "main_wallet"}, "23514"},
		{"a VASPRO account with a tenant", account{tenant: &tenant, typ: "revenue", service: ptr("sms")}, "23514"},
		{"a service wallet without a service", account{tenant: &tenant, typ: "service_wallet"}, "23514"},
		{"a revenue account without a service", account{typ: "revenue"}, "23514"},
		{"provider clearing without a rail", account{typ: "provider_clearing"}, "23514"},
		{"a rail on a main wallet", account{tenant: &tenant, typ: "main_wallet", rail: ptr("mpesa_ke"), currency: "UGX"}, "23514"},
		{"an unknown currency", account{typ: "suspense", currency: "USD"}, "23503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
				_, err := insertAccount(ctx, tx, tc.a)
				return err
			})
			wantCode(t, err, tc.code)
		})
	}

	t.Run("allowed: the same type in another currency, another service, a restricted bonus", func(t *testing.T) {
		for _, a := range []account{
			{typ: "suspense", currency: "UGX"},
			{tenant: &tenant, typ: "main_wallet", currency: "TZS"},
			{tenant: &tenant, typ: "service_wallet", service: ptr("sms")},
			{tenant: &tenant, typ: "service_wallet", service: ptr("whatsapp")},
			{tenant: &tenant, typ: "bonus"},
			{tenant: &tenant, typ: "bonus", service: ptr("sms")},
		} {
			if _, err := insertAccount(ctx, db.super, a); err != nil {
				t.Fatalf("%+v: %v", a, err)
			}
		}
	})

	t.Run("status", func(t *testing.T) {
		_, err := db.super.Exec(ctx, `UPDATE accounts SET status = 'dormant' WHERE tenant_id = $1`, tenant)
		wantCode(t, err, "23514")
	})
}

func TestTokenPrices_Constraints(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	client := uuid.New()
	maker, checker := uuid.New(), uuid.New()

	type price struct {
		tenant    *uuid.UUID
		minTokens string
		minor     string
		status    string
		from, to  string
		approver  *uuid.UUID
	}
	insert := func(q querier, p price) error {
		if p.minTokens == "" {
			p.minTokens = "0"
		}
		if p.minor == "" {
			p.minor = "80"
		}
		var to *string
		if p.to != "" {
			to = &p.to
		}
		_, err := q.Exec(ctx, `
			INSERT INTO token_prices (id, tenant_id, service_code, currency, min_tokens, minor_per_token, status, effective_from, effective_to, created_by, approved_by)
			VALUES ($1, $2, 'sms', 'KES', $3, $4, $5, $6, $7, $8, $9)`,
			uuid.New(), p.tenant, p.minTokens, p.minor, p.status, p.from, to, maker, p.approver)
		return err
	}
	inTx := func(p price) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error { return insert(tx, p) })
	}

	// A client deal from 1 Jan to 1 Apr, and the list price from 1 Jan
	// with no end.
	if err := insert(db.super, price{tenant: &client, status: "active", from: "2026-01-01", to: "2026-04-01", approver: &checker}); err != nil {
		t.Fatalf("deal: %v", err)
	}
	if err := insert(db.super, price{status: "active", from: "2026-01-01", approver: &checker}); err != nil {
		t.Fatalf("list price: %v", err)
	}

	refused := []struct {
		name string
		p    price
		code string
	}{
		{"active without an approver", price{tenant: &client, status: "active", from: "2026-06-01"}, "23514"},
		{"approved by its own maker", price{tenant: &client, status: "pending_approval", from: "2026-06-01", approver: &maker}, "23514"},
		{"a zero price", price{tenant: &client, minor: "0", status: "pending_approval", from: "2026-06-01"}, "23514"},
		{"an unknown status", price{tenant: &client, status: "draft", from: "2026-06-01"}, "23514"},
		{"a second active deal overlapping the first", price{tenant: &client, status: "active", from: "2026-03-01", to: "2026-05-01", approver: &checker}, "23P01"},
		{"a second active list price overlapping the first", price{status: "active", from: "2026-07-01", approver: &checker}, "23P01"},
	}
	for _, tc := range refused {
		t.Run("refused: "+tc.name, func(t *testing.T) { wantCode(t, inTx(tc.p), tc.code) })
	}

	allowed := []struct {
		name string
		p    price
	}{
		{"a deal starting where the first ends", price{tenant: &client, status: "active", from: "2026-04-01", approver: &checker}},
		{"an overlapping deal for another volume tier", price{tenant: &client, minTokens: "100000", minor: "70", status: "active", from: "2026-02-01", approver: &checker}},
		{"an overlapping deal still pending approval", price{tenant: &client, status: "pending_approval", from: "2026-02-01"}},
		{"an overlapping withdrawn deal", price{tenant: &client, status: "withdrawn", from: "2026-02-01"}},
		{"another client's overlapping deal", price{tenant: ptr(uuid.New()), status: "active", from: "2026-02-01", approver: &checker}},
	}
	for _, tc := range allowed {
		t.Run("allowed: "+tc.name, func(t *testing.T) {
			if err := inTx(tc.p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOtherTables_Constraints(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	price := listPrice(t, db, "0")

	var wallet, txn uuid.UUID
	if err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
		var err error
		wallet, txn, err = fundedServiceWallet(ctx, tx, tenant, 80000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ensureBalanceRows(t, db, tenant)
	holdRef := uuid.New()
	if _, err := db.super.Exec(ctx, `
		INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, status, expires_at)
		VALUES ($1, $2, $3, 'campaign', $4, 100, 'active', now())`, uuid.New(), tenant, wallet, holdRef); err != nil {
		t.Fatal(err)
	}
	if _, err := db.super.Exec(ctx, `
		INSERT INTO usage_batches (id, tenant_id, account_id, window_start, window_end, events, tokens)
		VALUES ($1, $2, $3, '2026-11-01 10:00Z', '2026-11-01 10:05Z', 1, 1)`, uuid.New(), tenant, wallet); err != nil {
		t.Fatal(err)
	}
	if _, err := db.super.Exec(ctx, `
		INSERT INTO auto_buy_rules (id, tenant_id, service_code, currency, below_tokens, buy_tokens)
		VALUES ($1, $2, 'sms', 'KES', 100, 1000)`, uuid.New(), tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.super.Exec(ctx, `INSERT INTO tax_rates (country_code, tax_code, rate, effective_from) VALUES ('KE', 'VAT', 0.1600, '2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	maker := uuid.New()

	cases := []struct {
		name string
		sql  string
		args []any
		code string
	}{
		{"a second hold for one reference", `
			INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, status, expires_at)
			VALUES ($1, $2, $3, 'campaign', $4, 100, 'active', now())`, []any{uuid.New(), tenant, wallet, holdRef}, "23505"},
		{"a zero hold", `
			INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, status, expires_at)
			VALUES ($1, $2, $3, 'campaign', $4, 0, 'active', now())`, []any{uuid.New(), tenant, wallet, uuid.New()}, "23514"},
		{"a hold in an unknown status", `
			INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, status, expires_at)
			VALUES ($1, $2, $3, 'campaign', $4, 1, 'pending', now())`, []any{uuid.New(), tenant, wallet, uuid.New()}, "23514"},
		{"an entry with a direction other than D or C", `
			INSERT INTO entries (transaction_id, account_id, direction, amount_minor, currency) VALUES ($1, $2, 'X', 1, 'KES')`,
			[]any{txn, wallet}, "23514"},
		{"a zero entry", `
			INSERT INTO entries (transaction_id, account_id, direction, amount_minor, currency) VALUES ($1, $2, 'C', 0, 'KES')`,
			[]any{txn, wallet}, "23514"},
		{"a negative entry", `
			INSERT INTO entries (transaction_id, account_id, direction, amount_minor, currency) VALUES ($1, $2, 'C', -5, 'KES')`,
			[]any{txn, wallet}, "23514"},
		{"a lot with negative tokens remaining", `
			INSERT INTO token_lots (id, tenant_id, account_id, transaction_id, tokens_bought, tokens_remaining, minor_per_token, token_price_id, purchased_at)
			VALUES ($1, $2, $3, $4, 10, -0.000001, 80, $5, now())`, []any{uuid.New(), tenant, wallet, txn, price}, "23514"},
		{"a second usage batch for one wallet and window", `
			INSERT INTO usage_batches (id, tenant_id, account_id, window_start, window_end, events, tokens)
			VALUES ($1, $2, $3, '2026-11-01 10:00Z', '2026-11-01 10:05Z', 1, 1)`, []any{uuid.New(), tenant, wallet}, "23505"},
		{"a second auto-buy rule for one service and currency", `
			INSERT INTO auto_buy_rules (id, tenant_id, service_code, currency, below_tokens, buy_tokens)
			VALUES ($1, $2, 'sms', 'KES', 5, 50)`, []any{uuid.New(), tenant}, "23505"},
		{"a reversal approved by its own maker", `
			INSERT INTO reversals (id, tenant_id, original_transaction_id, amount_minor, reason, status, requested_by, approved_by, kind, currency)
			VALUES ($1, $2, $3, 100, 'chargeback', 'approved', $4, $4, 'chargeback', 'KES')`, []any{uuid.New(), tenant, txn, maker}, "23514"},
		{"a zero reversal", `
			INSERT INTO reversals (id, tenant_id, original_transaction_id, amount_minor, reason, status, requested_by, kind, currency)
			VALUES ($1, $2, $3, 0, 'chargeback', 'requested', $4, 'chargeback', 'KES')`, []any{uuid.New(), tenant, txn, maker}, "23514"},
		{"a reversal in an unknown status", `
			INSERT INTO reversals (id, tenant_id, original_transaction_id, amount_minor, reason, status, requested_by, kind, currency)
			VALUES ($1, $2, $3, 1, 'chargeback', 'done', $4, 'chargeback', 'KES')`, []any{uuid.New(), tenant, txn, maker}, "23514"},
		{"a negative held amount", `UPDATE balances SET held_minor = -1 WHERE account_id = $1`, []any{wallet}, "23514"},
		{"a second VAT rate from the same day", `
			INSERT INTO tax_rates (country_code, tax_code, rate, effective_from) VALUES ('KE', 'VAT', 0.1500, '2026-01-01')`, nil, "23505"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, tc.sql, tc.args...)
				return err
			})
			wantCode(t, err, tc.code)
		})
	}

	t.Run("a reversal approved by someone else", func(t *testing.T) {
		if _, err := db.super.Exec(ctx, `
			INSERT INTO reversals (id, tenant_id, original_transaction_id, amount_minor, reason, status, requested_by, approved_by, kind, currency)
			VALUES ($1, $2, $3, 100, 'chargeback', 'approved', $4, $5, 'chargeback', 'KES')`, uuid.New(), tenant, txn, maker, uuid.New()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestTransactions_IdempotencyKeyUnique: one key per tenant, and one
// per system (NULL tenant, NULLS NOT DISTINCT).
func TestTransactions_IdempotencyKeyUnique(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	clearing := mustAccount(t, db.super, account{typ: "provider_clearing", rail: ptr("mpesa_ke")})
	suspense := mustAccount(t, db.super, account{typ: "suspense"})
	recv := mustAccount(t, db.super, account{tenant: &tenant, typ: "receivable"})
	wallet := mustAccount(t, db.super, account{tenant: &tenant, typ: "main_wallet"})

	postWithKey := func(tenant *uuid.UUID, key string, l ...line) error {
		return inSuperTx(ctx, db, func(tx pgx.Tx) error {
			id := uuid.New()
			if _, err := tx.Exec(ctx, `
				INSERT INTO transactions (id, tenant_id, type, idempotency_key, description)
				VALUES ($1, $2, 'topup', $3, 'test')`, id, tenant, key); err != nil {
				return err
			}
			for _, x := range l {
				if err := insertEntry(ctx, tx, id, x); err != nil {
					return err
				}
			}
			return nil
		})
	}

	sys := []line{debit(clearing, 1), credit(suspense, 1)}
	client := []line{debit(recv, 1), credit(wallet, 1)}
	if err := postWithKey(nil, "mpesa:ABC123", sys...); err != nil {
		t.Fatal(err)
	}
	wantCode(t, postWithKey(nil, "mpesa:ABC123", sys...), "23505")
	if err := postWithKey(&tenant, "mpesa:ABC123", client...); err != nil {
		t.Fatalf("the same key for a client: %v", err)
	}
	wantCode(t, postWithKey(&tenant, "mpesa:ABC123", client...), "23505")
}
