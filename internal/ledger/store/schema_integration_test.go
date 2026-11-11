//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx" database/sql driver, for goose's Down round trip only
	"github.com/pressly/goose/v3"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// TestMigration_DownThenUp proves the Down sections really undo Up:
// after DownTo(0) the schema and both roles are gone, and Up applies
// cleanly again.
func TestMigration_DownThenUp(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	if _, err := db.super.Exec(ctx, `DROP ROLE ledger_app`); err != nil {
		t.Fatalf("drop ledger_app: %v", err)
	}

	sqlDB, err := sql.Open("pgx", db.pg.DSN())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(migrationsDir))
	if err != nil {
		t.Fatalf("goose.NewProvider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 0); err != nil {
		t.Fatalf("DownTo(0): %v", err)
	}

	var schema, roles int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'ledger'`).Scan(&schema); err != nil {
		t.Fatalf("checking schema: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_roles WHERE rolname IN ('ledger_service', 'ledger_system')`).Scan(&roles); err != nil {
		t.Fatalf("checking roles: %v", err)
	}
	if schema != 0 || roles != 0 {
		t.Fatalf("after DownTo(0): %d ledger schema(s), %d ledger role(s) remain", schema, roles)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

// TestCurrencies_Seeded: KES and TZS have 2 decimals, UGX none (data
// model §5, platform/money).
func TestCurrencies_Seeded(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)

	rows, err := db.app.Query(ctx, `SELECT code, minor_exponent FROM currencies ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var code string
		var exp int
		err := r.Scan(&code, &exp)
		return fmt.Sprintf("%s:%d", code, exp), err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"KES:2", "TZS:2", "UGX:0"}
	if len(got) != len(want) {
		t.Fatalf("currencies = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("currencies = %v, want %v", got, want)
		}
	}
}

// TestAccountTypes_MatchDomain: the SQL enum, ledger.normal_side and
// ledger.is_client_wallet agree with domain's one table for every type
// (OQ-13: the sign convention lives in domain, and the triggers need a
// copy).
func TestAccountTypes_MatchDomain(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)

	rows, err := db.app.Query(ctx, `
		SELECT t::text, ledger.normal_side(t), ledger.is_client_wallet(t), ledger.is_client_owned(t)
		FROM unnest(enum_range(NULL::ledger.account_type)) AS t`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		typ                 string
		side                string
		wallet, clientOwned bool
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.typ, &x.side, &x.wallet, &x.clientOwned)
		return x, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(domain.AccountTypes) {
		t.Fatalf("enum has %d types, domain %d", len(got), len(domain.AccountTypes))
	}
	for i, r := range got {
		typ := domain.AccountTypes[i]
		if r.typ != string(typ) {
			t.Errorf("enum[%d] = %q, domain %q", i, r.typ, typ)
		}
		side, _ := domain.NormalSide(typ)
		if r.side != string(rune(side)) {
			t.Errorf("normal_side(%s) = %q, domain %q", typ, r.side, string(rune(side)))
		}
		if r.wallet != domain.IsClientWallet(typ) {
			t.Errorf("is_client_wallet(%s) = %v, domain %v", typ, r.wallet, domain.IsClientWallet(typ))
		}
		if r.clientOwned != domain.IsClientOwned(typ) {
			t.Errorf("is_client_owned(%s) = %v, domain %v", typ, r.clientOwned, domain.IsClientOwned(typ))
		}
	}
}

// TestRowLevelSecurity_TenantOwnedTables runs the canonical RLS check
// (data model §1) over the five tables with tenant_id NOT NULL.
func TestRowLevelSecurity_TenantOwnedTables(t *testing.T) {
	db := newLedgerDB(t)
	price := listPrice(t, db, "0")

	cases := []struct {
		table  string
		insert func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error
	}{
		{"token_lots", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			wallet, txn, err := fundedServiceWallet(ctx, tx, tenant, 80000)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO token_lots (id, tenant_id, account_id, transaction_id, tokens_bought, tokens_remaining, minor_per_token, token_price_id, purchased_at)
				VALUES ($1, $2, $3, $4, 1000, 1000, 80, $5, now())`, uuid.New(), tenant, wallet, txn, price)
			return err
		}},
		{"holds", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			wallet, _, err := fundedServiceWallet(ctx, tx, tenant, 80000)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO holds (id, tenant_id, account_id, reference_type, reference_id, amount_minor, tokens, status, expires_at)
				VALUES ($1, $2, $3, 'campaign', $4, 32000, 400, 'active', now() + interval '1 hour')`, uuid.New(), tenant, wallet, uuid.New())
			return err
		}},
		{"usage_batches", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			wallet, _, err := fundedServiceWallet(ctx, tx, tenant, 80000)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO usage_batches (id, tenant_id, account_id, window_start, window_end, events, tokens)
				VALUES ($1, $2, $3, '2026-11-01 10:00Z', '2026-11-01 10:05Z', 380, 380)`, uuid.New(), tenant, wallet)
			return err
		}},
		{"auto_buy_rules", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO auto_buy_rules (id, tenant_id, service_code, currency, below_tokens, buy_tokens)
				VALUES ($1, $2, 'sms', 'KES', 100, 10000)`, uuid.New(), tenant)
			return err
		}},
		{"reversals", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			_, txn, err := fundedServiceWallet(ctx, tx, tenant, 80000)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO reversals (id, tenant_id, original_transaction_id, amount_minor, reason, status, requested_by, kind, currency)
				VALUES ($1, $2, $3, 80000, 'chargeback', 'requested', $4, 'chargeback', 'KES')`, uuid.New(), tenant, txn, uuid.New())
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			testkit.AssertRowLevelSecurity(t, db.app, tc.insert,
				func(ctx context.Context, tx pgx.Tx) (int, error) {
					return count(ctx, tx, `SELECT count(*) FROM `+pgx.Identifier{tc.table}.Sanitize())
				},
			)
		})
	}
}

// TestRowLevelSecurity_SharedTables covers accounts, token_prices and
// rate_cards (OQ-13): a tenant reads its own rows and VASPRO's
// (tenant_id NULL) but never another tenant's, writes only its own, and
// NULL-tenant rows are written only by ledger_system inside WithRoleTx.
func TestRowLevelSecurity_SharedTables(t *testing.T) {
	ctx := context.Background()

	type table struct {
		name string
		// insert adds one row owned by tenant (nil = VASPRO's).
		insert func(ctx context.Context, q querier, tenant *uuid.UUID) error
	}
	tables := []table{
		{"accounts", func(ctx context.Context, q querier, tenant *uuid.UUID) error {
			typ := "suspense"
			if tenant != nil {
				typ = "main_wallet"
			}
			// A fresh currency each time would need seeding; distinct
			// rails keep VASPRO's rows unique instead.
			if tenant == nil {
				_, err := insertAccount(ctx, q, account{typ: "provider_clearing", rail: ptr(uuid.NewString())})
				return err
			}
			_, err := insertAccount(ctx, q, account{tenant: tenant, typ: typ})
			return err
		}},
		{"token_prices", func(ctx context.Context, q querier, tenant *uuid.UUID) error {
			_, err := q.Exec(ctx, `
				INSERT INTO token_prices (id, tenant_id, service_code, currency, minor_per_token, status, effective_from, created_by)
				VALUES ($1, $2, 'sms', 'KES', 80, 'pending_approval', now(), $3)`, uuid.New(), tenant, uuid.New())
			return err
		}},
		{"rate_cards", func(ctx context.Context, q querier, tenant *uuid.UUID) error {
			_, err := q.Exec(ctx, `
				INSERT INTO rate_cards (id, tenant_id, service_code, country_code, tokens_per_event, prices_include_tax, effective_from)
				VALUES ($1, $2, 'sms', 'KE', 1, true, now())`, uuid.New(), tenant)
			return err
		}},
	}

	for _, tb := range tables {
		t.Run(tb.name, func(t *testing.T) {
			db := newLedgerDB(t)
			a, b := uuid.New(), uuid.New()
			countAll := `SELECT count(*) FROM ` + pgx.Identifier{tb.name}.Sanitize()

			if err := tb.insert(ctx, db.super, nil); err != nil {
				t.Fatalf("superuser inserting VASPRO's row: %v", err)
			}
			for _, tenant := range []uuid.UUID{a, b} {
				if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error { return tb.insert(ctx, tx, &tenant) }); err != nil {
					t.Fatalf("tenant inserting its own row: %v", err)
				}
			}

			var seenByA, mineSeenByA int
			if err := asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
				var err error
				if seenByA, err = count(ctx, tx, countAll); err != nil {
					return err
				}
				mineSeenByA, err = count(ctx, tx, countAll+` WHERE tenant_id = $1`, a)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if seenByA != 2 || mineSeenByA != 1 {
				t.Fatalf("tenant A should see its own row and VASPRO's (2), saw %d (%d its own)", seenByA, mineSeenByA)
			}

			seenWithoutTenant, err := count(ctx, db.app, countAll+` WHERE tenant_id IS NOT NULL`)
			if err != nil {
				t.Fatal(err)
			}
			if seenWithoutTenant != 0 {
				t.Fatalf("no tenant set: saw %d client rows, want 0", seenWithoutTenant)
			}

			t.Run("a tenant can't write VASPRO's rows", func(t *testing.T) {
				err := asTenant(ctx, db.app, a, func(tx pgx.Tx) error { return tb.insert(ctx, tx, nil) })
				wantCode(t, err, "42501")

				var updated int64
				err = asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
					tag, err := tx.Exec(ctx, `UPDATE `+pgx.Identifier{tb.name}.Sanitize()+` SET tenant_id = tenant_id WHERE tenant_id IS NULL`)
					updated = tag.RowsAffected()
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if updated != 0 {
					t.Fatalf("tenant updated %d of VASPRO's rows", updated)
				}
			})

			t.Run("a tenant can't write another tenant's rows", func(t *testing.T) {
				err := asTenant(ctx, db.app, a, func(tx pgx.Tx) error { return tb.insert(ctx, tx, &b) })
				wantCode(t, err, "42501")

				var updated int64
				err = asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
					tag, err := tx.Exec(ctx, `UPDATE `+pgx.Identifier{tb.name}.Sanitize()+` SET tenant_id = tenant_id WHERE tenant_id = $1`, b)
					updated = tag.RowsAffected()
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if updated != 0 {
					t.Fatalf("tenant A updated %d of tenant B's rows", updated)
				}
			})

			t.Run("ledger_system writes VASPRO's rows only", func(t *testing.T) {
				if err := asSystem(ctx, db.app, func(tx pgx.Tx) error { return tb.insert(ctx, tx, nil) }); err != nil {
					t.Fatalf("ledger_system inserting VASPRO's row: %v", err)
				}
				err := asSystem(ctx, db.app, func(tx pgx.Tx) error { return tb.insert(ctx, tx, &a) })
				wantCode(t, err, "42501")
			})

			t.Run("outside WithRoleTx nobody writes VASPRO's rows", func(t *testing.T) {
				err := pgx.BeginFunc(ctx, db.app, func(tx pgx.Tx) error { return tb.insert(ctx, tx, nil) })
				wantCode(t, err, "42501")
			})
		})
	}
}

// TestRowLevelSecurity_Transactions: client postings are tenant rows;
// system postings (tenant_id NULL) only through ledger_system, and no
// tenant sees them.
func TestRowLevelSecurity_Transactions(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	a, b := uuid.New(), uuid.New()

	for _, tenant := range []uuid.UUID{a, b} {
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, _, err := fundedServiceWallet(ctx, tx, tenant, 1000)
			return err
		}); err != nil {
			t.Fatalf("tenant posting: %v", err)
		}
	}

	var clearing, suspense uuid.UUID
	if err := asSystem(ctx, db.app, func(tx pgx.Tx) error {
		var err error
		if clearing, err = insertAccount(ctx, tx, account{typ: "provider_clearing", rail: ptr("mpesa_ke")}); err != nil {
			return err
		}
		if suspense, err = insertAccount(ctx, tx, account{typ: "suspense"}); err != nil {
			return err
		}
		_, err = post(ctx, tx, nil, debit(clearing, 5000), credit(suspense, 5000))
		return err
	}); err != nil {
		t.Fatalf("ledger_system posting: %v", err)
	}

	var seen int
	if err := asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
		var err error
		seen, err = count(ctx, tx, `SELECT count(*) FROM transactions`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("tenant A sees %d transactions, want only its own 1", seen)
	}

	err := asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
		_, err := post(ctx, tx, nil, debit(clearing, 1), credit(suspense, 1))
		return err
	})
	wantCode(t, err, "42501")

	err = asSystem(ctx, db.app, func(tx pgx.Tx) error {
		_, err := post(ctx, tx, &a, debit(clearing, 1), credit(suspense, 1))
		return err
	})
	wantCode(t, err, "42501")
}

// TestRowLevelSecurity_Entries: a client posting may use the tenant's
// own accounts and VASPRO's (revenue, tax), never another tenant's. A
// tenant sees the lines of its own postings only, including their
// VASPRO lines, and never another tenant's lines on VASPRO accounts.
func TestRowLevelSecurity_Entries(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	a, b := uuid.New(), uuid.New()

	revenue := mustAccount(t, db.super, account{typ: "revenue", service: ptr("sms")})
	receivable := map[uuid.UUID]uuid.UUID{}
	for _, tenant := range []uuid.UUID{a, b} {
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			recv, err := insertAccount(ctx, tx, account{tenant: &tenant, typ: "receivable"})
			if err != nil {
				return err
			}
			receivable[tenant] = recv
			_, err = post(ctx, tx, &tenant, debit(recv, 700), credit(revenue, 700))
			return err
		}); err != nil {
			t.Fatalf("tenant posting to VASPRO's revenue: %v", err)
		}
	}

	var seenByA, revenueSeenByA int
	if err := asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
		var err error
		if seenByA, err = count(ctx, tx, `SELECT count(*) FROM entries`); err != nil {
			return err
		}
		revenueSeenByA, err = count(ctx, tx, `SELECT count(*) FROM entries WHERE account_id = $1`, revenue)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seenByA != 2 || revenueSeenByA != 1 {
		t.Fatalf("tenant A sees %d lines (%d on revenue), want its own 2 (1 on revenue)", seenByA, revenueSeenByA)
	}

	noTenant, err := count(ctx, db.app, `SELECT count(*) FROM entries`)
	if err != nil {
		t.Fatal(err)
	}
	if noTenant != 0 {
		t.Fatalf("no tenant set: saw %d lines, want 0", noTenant)
	}

	err = asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
		_, err := post(ctx, tx, &a, debit(receivable[a], 10), credit(receivable[b], 10))
		return err
	})
	wantCode(t, err, "42501")
}

// TestRowLevelSecurity_Balances: a client account's balance row is
// visible to its tenant only.
func TestRowLevelSecurity_Balances(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	a, b := uuid.New(), uuid.New()

	for _, tenant := range []uuid.UUID{a, b} {
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, _, err := fundedServiceWallet(ctx, tx, tenant, 1000)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		ensureBalanceRows(t, db, tenant)
	}

	var seenByA, foreign int
	if err := asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
		var err error
		if seenByA, err = count(ctx, tx, `SELECT count(*) FROM balances`); err != nil {
			return err
		}
		foreign, err = count(ctx, tx, `
			SELECT count(*) FROM balances bl
			WHERE NOT EXISTS (SELECT 1 FROM accounts ac WHERE ac.id = bl.account_id AND ac.tenant_id = $1)`, a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seenByA != 2 || foreign != 0 {
		t.Fatalf("tenant A sees %d balance rows (%d not its own), want its own 2", seenByA, foreign)
	}

	noTenant, err := count(ctx, db.app, `SELECT count(*) FROM balances`)
	if err != nil {
		t.Fatal(err)
	}
	if noTenant != 0 {
		t.Fatalf("no tenant set: saw %d balance rows, want 0", noTenant)
	}
}

// TestRowLevelSecurity_LotConsumptions: lot_consumptions has no
// tenant_id; it follows its lot.
func TestRowLevelSecurity_LotConsumptions(t *testing.T) {
	ctx := context.Background()
	db := newLedgerDB(t)
	price := listPrice(t, db, "0")
	a, b := uuid.New(), uuid.New()

	lotOf, txnOf := map[uuid.UUID]uuid.UUID{}, map[uuid.UUID]uuid.UUID{}
	for _, tenant := range []uuid.UUID{a, b} {
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			wallet, txn, err := fundedServiceWallet(ctx, tx, tenant, 80000)
			if err != nil {
				return err
			}
			lot := uuid.New()
			lotOf[tenant], txnOf[tenant] = lot, txn
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
	}

	var seenByA int
	if err := asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
		var err error
		seenByA, err = count(ctx, tx, `SELECT count(*) FROM lot_consumptions`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seenByA != 1 {
		t.Fatalf("tenant A sees %d consumptions, want its own 1", seenByA)
	}

	err := asTenant(ctx, db.app, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO lot_consumptions (lot_id, transaction_id, tokens, value_minor) VALUES ($1, $2, 1, 80)`, lotOf[b], txnOf[a])
		return err
	})
	wantCode(t, err, "42501")
}
