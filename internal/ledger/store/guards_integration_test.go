//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/money"
)

// Task 3.2's database guards. Most run as ledger_app inside a tenant
// transaction, as the service will; the immutability tests also run as
// the superuser table owner, whom only the triggers can stop.

func wantErr(t *testing.T, err error, code, contains string) {
	t.Helper()
	wantCode(t, err, code)
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("error %q does not mention %q", err, contains)
	}
}

// tenantTx opens a ledger_app transaction for tenant and leaves commit
// to the test, so a test can tell an insert failing from a commit
// failing.
func tenantTx(t *testing.T, db ledgerDB, tenant uuid.UUID) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := db.app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
		t.Fatal(err)
	}
	return tx
}

// clientAccounts creates a receivable and a KES main wallet for tenant.
func clientAccounts(t *testing.T, db ledgerDB, tenant uuid.UUID) (recv, main uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		var err error
		if recv, err = insertAccount(ctx, tx, account{tenant: &tenant, typ: "receivable"}); err != nil {
			return err
		}
		main, err = insertAccount(ctx, tx, account{tenant: &tenant, typ: "main_wallet"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return recv, main
}

func balanceOf(t *testing.T, db ledgerDB, acct uuid.UUID) (balance, held, version int64, found bool) {
	t.Helper()
	err := db.super.QueryRow(context.Background(),
		`SELECT balance_minor, held_minor, version FROM balances WHERE account_id = $1`, acct).Scan(&balance, &held, &version)
	if err == pgx.ErrNoRows {
		return 0, 0, 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return balance, held, version, true
}

// replayed is an account's balance rebuilt from its entries on its
// normal side: what the nightly job (3.8) will compute.
func replayed(t *testing.T, db ledgerDB, acct uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := db.super.QueryRow(context.Background(), `
		SELECT coalesce(sum(CASE WHEN e.direction = ledger.normal_side(a.type) THEN e.amount_minor ELSE -e.amount_minor END), 0)
		FROM entries e JOIN accounts a ON a.id = e.account_id WHERE e.account_id = $1`, acct).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBalanceGuard_CheckedAtCommit(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	recv, main := clientAccounts(t, db, tenant)
	var recvUGX, mainUGX uuid.UUID
	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		var err error
		if recvUGX, err = insertAccount(ctx, tx, account{tenant: &tenant, typ: "receivable", currency: "UGX"}); err != nil {
			return err
		}
		mainUGX, err = insertAccount(ctx, tx, account{tenant: &tenant, typ: "main_wallet", currency: "UGX"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ugx := func(l line) line { l.currency = "UGX"; return l }

	t.Run("an unbalanced transaction is accepted line by line and refused at commit", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		if _, err := post(ctx, tx, &tenant, debit(recv, 10000), credit(main, 9999)); err != nil {
			t.Fatalf("the lines themselves should insert: %v", err)
		}
		wantErr(t, tx.Commit(ctx), "23514", "does not balance")
	})

	t.Run("a balanced transaction commits", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		if _, err := post(ctx, tx, &tenant, debit(recv, 10000), credit(main, 6000), credit(main, 4000)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("balancing across currencies is refused: each currency balances on its own", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		if _, err := post(ctx, tx, &tenant, debit(recv, 100), ugx(credit(mainUGX, 100))); err != nil {
			t.Fatal(err)
		}
		wantErr(t, tx.Commit(ctx), "23514", "does not balance")
	})

	t.Run("two currencies each balanced commit", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		if _, err := post(ctx, tx, &tenant, debit(recv, 100), credit(main, 100), ugx(debit(recvUGX, 5)), ugx(credit(mainUGX, 5))); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a transaction with no lines is refused at commit", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		if _, err := post(ctx, tx, &tenant); err != nil {
			t.Fatal(err)
		}
		wantErr(t, tx.Commit(ctx), "23514", "no debit or no credit")
	})

	t.Run("a line in another currency than its account is refused", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		_, err := post(ctx, tx, &tenant, ugx(debit(recv, 100)))
		wantErr(t, err, "23514", "currency")
	})
}

// TestBalanceGuard_EntriesOnlyWithTheirTransaction: a posted
// transaction can't gain lines later, even balanced ones; a savepoint
// holding both the transaction and its lines is fine.
func TestBalanceGuard_EntriesOnlyWithTheirTransaction(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	recv, main := clientAccounts(t, db, tenant)

	var posted uuid.UUID
	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		var err error
		posted, err = post(ctx, tx, &tenant, debit(recv, 100), credit(main, 100))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("a balanced pair added to a posted transaction is refused", func(t *testing.T) {
		err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			if err := insertEntry(ctx, tx, posted, debit(recv, 50)); err != nil {
				return err
			}
			return insertEntry(ctx, tx, posted, credit(main, 50))
		})
		wantErr(t, err, "23514", "same transaction")
	})

	t.Run("as the table owner too", func(t *testing.T) {
		err := inSuperTx(ctx, db, func(tx pgx.Tx) error { return insertEntry(ctx, tx, posted, debit(recv, 50)) })
		wantErr(t, err, "23514", "same transaction")
	})

	t.Run("a transaction and its lines inside one savepoint commit", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := post(ctx, sp, &tenant, debit(recv, 10), credit(main, 10)); err != nil {
			t.Fatal(err)
		}
		if err := sp.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("lines in a different savepoint from their transaction are refused", func(t *testing.T) {
		tx := tenantTx(t, db, tenant)
		txn, err := post(ctx, tx, &tenant)
		if err != nil {
			t.Fatal(err)
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = insertEntry(ctx, sp, txn, debit(recv, 10))
		wantErr(t, err, "23514", "same transaction")
	})
}

// TestJournal_AppendOnly: transactions, entries and lot_consumptions
// can't be updated, deleted or truncated, by the table owner (triggers)
// or by the service's role (grants).
func TestJournal_AppendOnly(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	price := listPrice(t, db, "0")

	var txn, lot uuid.UUID
	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		wallet, t1, err := fundedServiceWallet(ctx, tx, tenant, 80000)
		if err != nil {
			return err
		}
		txn, lot = t1, uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO token_lots (id, tenant_id, account_id, transaction_id, tokens_bought, tokens_remaining, minor_per_token, token_price_id, purchased_at)
			VALUES ($1, $2, $3, $4, 1000, 1000, 80, $5, now())`, lot, tenant, wallet, txn, price); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO lot_consumptions (lot_id, transaction_id, tokens, value_minor) VALUES ($1, $2, 10, 800)`, lot, txn)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	statements := []struct {
		name, sql string
		args      []any
	}{
		{"UPDATE entries", `UPDATE entries SET amount_minor = amount_minor + 1 WHERE transaction_id = $1`, []any{txn}},
		{"DELETE entries", `DELETE FROM entries WHERE transaction_id = $1`, []any{txn}},
		{"TRUNCATE entries", `TRUNCATE entries`, nil},
		{"UPDATE transactions", `UPDATE transactions SET description = 'edited' WHERE id = $1`, []any{txn}},
		{"DELETE transactions", `DELETE FROM transactions WHERE id = $1`, []any{txn}},
		{"TRUNCATE transactions", `TRUNCATE transactions CASCADE`, nil},
		{"UPDATE lot_consumptions", `UPDATE lot_consumptions SET value_minor = 0 WHERE lot_id = $1`, []any{lot}},
		{"DELETE lot_consumptions", `DELETE FROM lot_consumptions WHERE lot_id = $1`, []any{lot}},
		{"TRUNCATE lot_consumptions", `TRUNCATE lot_consumptions`, nil},
	}
	for _, s := range statements {
		t.Run("owner: "+s.name, func(t *testing.T) {
			err := inSuperTx(ctx, db, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, s.sql, s.args...)
				return err
			})
			wantErr(t, err, "P0001", "append-only")
		})
		t.Run("ledger_service: "+s.name, func(t *testing.T) {
			err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, s.sql, s.args...)
				return err
			})
			wantCode(t, err, "42501")
		})
	}

	var lines int
	if err := db.super.QueryRow(ctx, `SELECT count(*) FROM entries WHERE transaction_id = $1`, txn).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if lines != 2 {
		t.Fatalf("the posting has %d lines after the attempts, want 2", lines)
	}
}

// TestBalanceSync: the entries trigger keeps client balances in step
// on each account's normal side; VASPRO's accounts have no balance row
// (ADR 0010); the service can't write balance_minor itself.
func TestBalanceSync(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	revenue := mustAccount(t, db.super, account{typ: "revenue", service: ptr("sms")})
	recv, main := clientAccounts(t, db, tenant)

	for _, acct := range []uuid.UUID{recv, main} {
		if b, h, v, found := balanceOf(t, db, acct); !found || b != 0 || h != 0 || v != 0 {
			t.Fatalf("a new client account should have a zero balance row, got found=%v %d/%d v%d", found, b, h, v)
		}
	}
	if _, _, _, found := balanceOf(t, db, revenue); found {
		t.Fatal("VASPRO's revenue account has a balance row")
	}

	postAs := func(lines ...line) {
		t.Helper()
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := post(ctx, tx, &tenant, lines...)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	postAs(debit(recv, 100000), credit(main, 100000))
	if b, _, v, _ := balanceOf(t, db, main); b != 100000 || v != 1 {
		t.Fatalf("main wallet after a credit: %d v%d, want 100000 v1", b, v)
	}
	if b, _, _, _ := balanceOf(t, db, recv); b != 100000 {
		t.Fatalf("receivable after a debit: %d, want 100000 (debit-normal)", b)
	}

	postAs(debit(main, 30000), credit(recv, 30000))
	if b, _, v, _ := balanceOf(t, db, main); b != 70000 || v != 2 {
		t.Fatalf("main wallet after a debit: %d v%d, want 70000 v2", b, v)
	}

	postAs(debit(main, 500), credit(revenue, 500))
	if _, _, _, found := balanceOf(t, db, revenue); found {
		t.Fatal("a posting to VASPRO's revenue created a balance row")
	}
	for _, acct := range []uuid.UUID{recv, main} {
		b, _, _, _ := balanceOf(t, db, acct)
		if r := replayed(t, db, acct); r != b {
			t.Fatalf("balance %d differs from the replay of its entries %d", b, r)
		}
	}

	t.Run("the service can't write balance_minor or create balance rows", func(t *testing.T) {
		err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE balances SET balance_minor = 999999999 WHERE account_id = $1`, main)
			return err
		})
		wantCode(t, err, "42501")
		err = asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO balances (account_id) VALUES ($1)`, uuid.New())
			return err
		})
		wantCode(t, err, "42501")
	})

	t.Run("the service may move held_minor", func(t *testing.T) {
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE balances SET held_minor = 1000, version = version + 1 WHERE account_id = $1`, main)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestWalletGuard: main, service and bonus wallets never go below zero
// available, checked at commit; other accounts may.
func TestWalletGuard(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	revenue := mustAccount(t, db.super, account{typ: "revenue", service: ptr("sms")})

	wallets := []account{
		{typ: "main_wallet"},
		{typ: "service_wallet", service: ptr("sms")},
		{typ: "bonus"},
	}
	for _, w := range wallets {
		t.Run(w.typ+" can't be debited below zero", func(t *testing.T) {
			tenant := uuid.New()
			w.tenant = &tenant
			var wallet, recv uuid.UUID
			if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
				var err error
				if wallet, err = insertAccount(ctx, tx, w); err != nil {
					return err
				}
				if recv, err = insertAccount(ctx, tx, account{tenant: &tenant, typ: "receivable"}); err != nil {
					return err
				}
				_, err = post(ctx, tx, &tenant, debit(recv, 1000), credit(wallet, 1000))
				return err
			}); err != nil {
				t.Fatal(err)
			}

			tx := tenantTx(t, db, tenant)
			if _, err := post(ctx, tx, &tenant, debit(wallet, 1001), credit(revenue, 1001)); err != nil {
				t.Fatalf("the lines themselves should insert: %v", err)
			}
			wantErr(t, tx.Commit(ctx), "23514", "below zero")

			if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
				_, err := post(ctx, tx, &tenant, debit(wallet, 1000), credit(revenue, 1000))
				return err
			}); err != nil {
				t.Fatalf("spending exactly the balance should commit: %v", err)
			}
		})
	}

	t.Run("held can't exceed the balance", func(t *testing.T) {
		tenant := uuid.New()
		recv, main := clientAccounts(t, db, tenant)
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := post(ctx, tx, &tenant, debit(recv, 1000), credit(main, 1000))
			return err
		}); err != nil {
			t.Fatal(err)
		}

		tx := tenantTx(t, db, tenant)
		if _, err := tx.Exec(ctx, `UPDATE balances SET held_minor = 1001 WHERE account_id = $1`, main); err != nil {
			t.Fatal(err)
		}
		wantErr(t, tx.Commit(ctx), "23514", "below zero")

		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE balances SET held_minor = 1000 WHERE account_id = $1`, main)
			return err
		}); err != nil {
			t.Fatalf("holding exactly the balance should commit: %v", err)
		}

		tx = tenantTx(t, db, tenant)
		if _, err := post(ctx, tx, &tenant, debit(main, 1), credit(revenue, 1)); err != nil {
			t.Fatal(err)
		}
		wantErr(t, tx.Commit(ctx), "23514", "below zero")
	})

	t.Run("a posting may dip a wallet between lines if it ends at or above zero", func(t *testing.T) {
		tenant := uuid.New()
		recv, main := clientAccounts(t, db, tenant)
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := post(ctx, tx, &tenant, debit(main, 300), credit(revenue, 300), debit(recv, 500), credit(main, 500))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if b, _, _, _ := balanceOf(t, db, main); b != 200 {
			t.Fatalf("main wallet = %d, want 200", b)
		}
	})

	t.Run("receivables and payables may go below zero", func(t *testing.T) {
		tenant := uuid.New()
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			recv, err := insertAccount(ctx, tx, account{tenant: &tenant, typ: "receivable"})
			if err != nil {
				return err
			}
			payable, err := insertAccount(ctx, tx, account{tenant: &tenant, typ: "commission_payable"})
			if err != nil {
				return err
			}
			_, err = post(ctx, tx, &tenant, debit(payable, 100), credit(recv, 100))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("VASPRO's accounts may go below zero", func(t *testing.T) {
		if err := asSystem(ctx, db.app, func(tx pgx.Tx) error {
			suspense, err := insertAccount(ctx, tx, account{typ: "suspense"})
			if err != nil {
				return err
			}
			clearing, err := insertAccount(ctx, tx, account{typ: "reversal_clearing"})
			if err != nil {
				return err
			}
			_, err = post(ctx, tx, nil, debit(suspense, 100), credit(clearing, 100))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestWorkedExample_Postings posts data model §5's worked example
// (steps 1, 2 and 4, KES 0.80 deal, VAT 16% included) as raw entries:
// the schema-level slice; the golden test proper comes with 3.4/3.5.
func TestWorkedExample_Postings(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)

	clearing := mustAccount(t, db.super, account{typ: "provider_clearing", rail: ptr("mpesa_ke")})
	revenue := mustAccount(t, db.super, account{typ: "revenue", service: ptr("sms")})
	vat := mustAccount(t, db.super, account{typ: "tax_payable"})

	// 304.00 for 380 units at 80 cents, VAT inclusive: 262.07 + 41.93.
	usage := money.New(30400, money.KES)
	net, tax, err := money.SplitVATInclusive(usage, 1600)
	if err != nil {
		t.Fatal(err)
	}
	if net.Minor() != 26207 || tax.Minor() != 4193 {
		t.Fatalf("SplitVATInclusive(304.00) = %s + %s, want 262.07 + 41.93", net, tax)
	}

	tenant := uuid.New()
	var main, sms uuid.UUID
	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		var err error
		if main, err = insertAccount(ctx, tx, account{tenant: &tenant, typ: "main_wallet"}); err != nil {
			return err
		}
		if sms, err = insertAccount(ctx, tx, account{tenant: &tenant, typ: "service_wallet", service: ptr("sms")}); err != nil {
			return err
		}
		// 1. Top-up by M-Pesa.
		if _, err := post(ctx, tx, &tenant, debit(clearing, 1300000), credit(main, 1300000)); err != nil {
			return err
		}
		// 2. 10,000 SMS units at the 0.80 deal.
		if _, err := post(ctx, tx, &tenant, debit(main, 800000), credit(sms, 800000)); err != nil {
			return err
		}
		// 4. Settlement: 380 delivered.
		_, err = post(ctx, tx, &tenant, debit(sms, usage.Minor()), credit(revenue, net.Minor()), credit(vat, tax.Minor()))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	want := map[uuid.UUID]int64{main: 500000, sms: 769600}
	for acct, w := range want {
		if b, _, _, _ := balanceOf(t, db, acct); b != w {
			t.Errorf("balance = %d, want %d", b, w)
		}
	}
	vasp := map[uuid.UUID]int64{clearing: 1300000, revenue: 26207, vat: 4193}
	for acct, w := range vasp {
		if r := replayed(t, db, acct); r != w {
			t.Errorf("VASPRO account replays to %d, want %d", r, w)
		}
	}

	t.Run("tax-exclusive variant debits 352.64", func(t *testing.T) {
		total, tax, err := money.AddVATExclusive(money.New(30400, money.KES), 1600)
		if err != nil {
			t.Fatal(err)
		}
		if total.Minor() != 35264 || tax.Minor() != 4864 {
			t.Fatalf("AddVATExclusive(304.00) = %s (VAT %s), want 352.64 (48.64)", total, tax)
		}
		tenant := uuid.New()
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			sms, err := insertAccount(ctx, tx, account{tenant: &tenant, typ: "service_wallet", service: ptr("sms")})
			if err != nil {
				return err
			}
			if _, err := post(ctx, tx, &tenant, debit(clearing, 800000), credit(sms, 800000)); err != nil {
				return err
			}
			_, err = post(ctx, tx, &tenant, debit(sms, total.Minor()), credit(revenue, 30400), credit(vat, tax.Minor()))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestZeroExponentCurrency: UGX amounts are whole shillings in minor
// units and post and balance like any other.
func TestZeroExponentCurrency(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	tenant := uuid.New()
	var main uuid.UUID
	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		recv, err := insertAccount(ctx, tx, account{tenant: &tenant, typ: "receivable", currency: "UGX"})
		if err != nil {
			return err
		}
		if main, err = insertAccount(ctx, tx, account{tenant: &tenant, typ: "main_wallet", currency: "UGX"}); err != nil {
			return err
		}
		_, err = post(ctx, tx, &tenant,
			line{recv, "D", 50000, "UGX"}, line{main, "C", 49999, "UGX"}, line{main, "C", 1, "UGX"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if b, _, _, _ := balanceOf(t, db, main); b != 50000 {
		t.Fatalf("UGX main wallet = %d, want 50000", b)
	}
}
