//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx" database/sql driver, for goose's Down round trip only
	"github.com/pressly/goose/v3"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

const rlsViolation = "42501" // also insufficient_privilege

// TestMigration_DownThenUp: Down really undoes Up, schema and roles.
func TestMigration_DownThenUp(t *testing.T) {
	ctx := context.Background()
	db := newPaymentsDB(t)
	if _, err := db.super.Exec(ctx, `DROP ROLE payments_app`); err != nil {
		t.Fatalf("drop payments_app: %v", err)
	}
	sqlDB, err := sql.Open("pgx", db.pg.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(migrationsDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 0); err != nil {
		t.Fatalf("DownTo(0): %v", err)
	}
	var schema, roles int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'payments'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_roles WHERE rolname IN ('payments_service', 'payments_system', 'payments_operations')`).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if schema != 0 || roles != 0 {
		t.Fatalf("after DownTo(0): %d payments schema(s), %d payments role(s) remain", schema, roles)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

func TestRLS_StandardTables(t *testing.T) {
	db := newPaymentsDB(t)
	pb := seedPaybill(t, db)

	t.Run("payment_intents", func(t *testing.T) {
		testkit.AssertRowLevelSecurity(t, db.app,
			func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
				return insert(ctx, tx, "payment_intents", intent(tenant, pb))
			},
			func(ctx context.Context, tx pgx.Tx) (int, error) {
				return count(ctx, tx, `SELECT count(*) FROM payment_intents`), nil
			})
	})
	t.Run("topup_accounts", func(t *testing.T) {
		numbers := []string{"ABC23456", "XYZ23456"}
		testkit.AssertRowLevelSecurity(t, db.app,
			func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
				n := numbers[0]
				numbers = numbers[1:]
				return insert(ctx, tx, "topup_accounts", topupAccount(tenant, pb, n))
			},
			func(ctx context.Context, tx pgx.Tx) (int, error) {
				return count(ctx, tx, `SELECT count(*) FROM topup_accounts`), nil
			})
	})
	t.Run("payments_system reads every tenant's intents and numbers", func(t *testing.T) {
		if n := roleCount(t, db, systemRole, `SELECT count(*) FROM payment_intents`); n != 2 {
			t.Fatalf("intents %d, want 2", n)
		}
		if n := roleCount(t, db, systemRole, `SELECT count(*) FROM topup_accounts`); n != 2 {
			t.Fatalf("numbers %d, want 2", n)
		}
		err := asRole(context.Background(), db.app, systemRole, func(tx pgx.Tx) error {
			return insert(context.Background(), tx, "payment_intents", intent(uuid.New(), pb))
		})
		wantCode(t, err, rlsViolation)
	})
}

func TestRLS_RailAccounts(t *testing.T) {
	ctx := context.Background()
	db := newPaymentsDB(t)
	pb := seedPaybill(t, db)
	tenantA, tenantB := uuid.New(), uuid.New()

	if err := asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
		return insert(ctx, tx, "rail_accounts", clientPaybill(tenantA, "700200"))
	}); err != nil {
		t.Fatalf("a tenant adds its own account: %v", err)
	}
	if n := tenantCount(t, db, tenantA, `SELECT count(*) FROM rail_accounts`); n != 2 {
		t.Fatalf("A sees %d accounts, want its own and VASPRO's", n)
	}
	if n := tenantCount(t, db, tenantB, `SELECT count(*) FROM rail_accounts`); n != 1 {
		t.Fatalf("B sees %d accounts, want VASPRO's only", n)
	}
	err := asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
		return insert(ctx, tx, "rail_accounts", with(vasproPaybill(), row{"number": "600999"}))
	})
	wantCode(t, err, rlsViolation)
	err = asTenant(ctx, db.app, tenantB, func(tx pgx.Tx) error {
		return insert(ctx, tx, "rail_accounts", clientPaybill(tenantA, "700299"))
	})
	wantCode(t, err, rlsViolation)
	if err := asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE rail_accounts SET verified_at = now() WHERE id = $1`, pb)
		if err == nil && tag.RowsAffected() != 0 {
			t.Fatal("a tenant changed VASPRO's paybill")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		return insert(ctx, tx, "rail_accounts", with(vasproPaybill(), row{"number": "600102"}))
	}); err != nil {
		t.Fatalf("payments_system adds VASPRO's account: %v", err)
	}
	err = asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		return insert(ctx, tx, "rail_accounts", clientPaybill(tenantB, "700300"))
	})
	wantCode(t, err, rlsViolation)
}

func TestRLS_RailCredentials(t *testing.T) {
	ctx := context.Background()
	db := newPaymentsDB(t)
	pb := seedPaybill(t, db)
	tenantA, tenantB := uuid.New(), uuid.New()
	cpb := clientPaybill(tenantA, "700200")
	mustInsert(t, db.super, "rail_accounts", cpb)
	mustInsert(t, db.super, "rail_credentials", credential(pb, nil))

	if err := asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
		return insert(ctx, tx, "rail_credentials", credential(cpb["id"].(uuid.UUID), tenantA))
	}); err != nil {
		t.Fatalf("a tenant stores its own credential: %v", err)
	}
	if n := tenantCount(t, db, tenantA, `SELECT count(*) FROM rail_credentials`); n != 1 {
		t.Fatalf("A sees %d credentials, want its own only", n)
	}
	if n := tenantCount(t, db, tenantB, `SELECT count(*) FROM rail_credentials`); n != 0 {
		t.Fatalf("B sees %d credentials, want none", n)
	}
	if n := roleCount(t, db, systemRole, `SELECT count(*) FROM rail_credentials`); n != 1 {
		t.Fatalf("payments_system sees %d credentials, want VASPRO's only", n)
	}
	// Operations staff have no grant on credentials at all.
	wantCode(t, asRole(ctx, db.app, operationsRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM rail_credentials`)
		return err
	}), rlsViolation)
	err := asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
		return insert(ctx, tx, "rail_credentials", with(credential(pb, nil), row{"kind": "passkey"}))
	})
	wantCode(t, err, rlsViolation)
}

func TestRLS_CollectionNotices(t *testing.T) {
	ctx := context.Background()
	db := newPaymentsDB(t)
	pb := seedPaybill(t, db)
	tenantA, tenantB := uuid.New(), uuid.New()
	cpbB := clientPaybill(tenantB, "700200")
	mustInsert(t, db.super, "rail_accounts", cpbB)
	mustInsert(t, db.super, "collection_notices", suspenseNotice(pb, "SUSPENSE01"))
	matched := matchedNotice(pb, tenantA, "MATCHED001")
	mustInsert(t, db.super, "collection_notices", matched)
	mustInsert(t, db.super, "collection_notices", with(suspenseNotice(cpbB["id"].(uuid.UUID), "CLIENTB001"), row{"tenant_id": tenantB, "match_status": "client_owned"}))

	visible := func(t *testing.T, got int, want int, who string) {
		t.Helper()
		if got != want {
			t.Fatalf("%s sees %d notices, want %d", who, got, want)
		}
	}
	const all = `SELECT count(*) FROM collection_notices`
	visible(t, tenantCount(t, db, tenantA, all), 1, "A (its matched top-up)")
	visible(t, tenantCount(t, db, tenantB, all), 1, "B (its own collection)")
	visible(t, roleCount(t, db, systemRole, all), 2, "payments_system (VASPRO's paybill)")
	visible(t, roleCount(t, db, operationsRole, all), 3, "operations")
	visible(t, count(ctx, db.app, all), 0, "the service outside a transaction")

	t.Run("a tenant records its own top-up only", func(t *testing.T) {
		wantCode(t, asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
			return insert(ctx, tx, "collection_notices", matchedNotice(pb, tenantA, "MATCHED002"))
		}), "")
		wantCode(t, asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
			return insert(ctx, tx, "collection_notices", matchedNotice(pb, tenantB, "MATCHED003"))
		}), rlsViolation)
		wantCode(t, asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
			return insert(ctx, tx, "collection_notices", suspenseNotice(pb, "SUSPENSE02"))
		}), rlsViolation)
	})
	t.Run("a tenant updates its matched notice, never suspense", func(t *testing.T) {
		wantCode(t, asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE collection_notices SET ledger_transaction_id = $1 WHERE id = $2`, uuid.New(), matched["id"])
			if err == nil && tag.RowsAffected() != 1 {
				t.Fatalf("updated %d rows, want 1", tag.RowsAffected())
			}
			return err
		}), "")
		wantCode(t, asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE collection_notices SET match_status = 'matched', matched_tenant_id = $1 WHERE rail_receipt = 'SUSPENSE01'`, tenantA)
			if err == nil && tag.RowsAffected() != 0 {
				t.Fatal("a tenant claimed a suspense payment")
			}
			return err
		}), "")
	})
	t.Run("payments_system matches suspense", func(t *testing.T) {
		wantCode(t, asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE collection_notices SET match_status = 'matched', matched_tenant_id = $1 WHERE rail_receipt = 'SUSPENSE01'`, tenantB)
			if err == nil && tag.RowsAffected() != 1 {
				t.Fatalf("matched %d rows, want 1", tag.RowsAffected())
			}
			return err
		}), "")
		visible(t, tenantCount(t, db, tenantB, all), 2, "B after the match")
	})
	t.Run("a duplicate receipt is caught even when the first is invisible", func(t *testing.T) {
		mustInsert(t, db.super, "collection_notices", suspenseNotice(pb, "HIDDEN0001"))
		wantCode(t, asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
			return insert(ctx, tx, "collection_notices", matchedNotice(pb, tenantA, "HIDDEN0001"))
		}), uniqueViolation)
		wantCode(t, asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
			n := matchedNotice(pb, tenantA, "HIDDEN0001")
			tag, err := tx.Exec(ctx, `
				INSERT INTO collection_notices (id, rail_account_id, rail_receipt, amount_minor, currency, paid_at, match_status, matched_tenant_id, raw)
				VALUES ($1, $2, 'HIDDEN0001', 100000, 'KES', now(), 'matched', $3, '{}')
				ON CONFLICT (rail_account_id, rail_receipt) DO NOTHING`, n["id"], pb, tenantA)
			if err == nil && tag.RowsAffected() != 0 {
				t.Fatal("the duplicate was inserted")
			}
			return err
		}), "")
		wantCode(t, asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
			return insert(ctx, tx, "collection_notices", suspenseNotice(pb, "HIDDEN0001"))
		}), uniqueViolation)
	})
}

func TestRLS_StatementsAndExceptions(t *testing.T) {
	ctx := context.Background()
	db := newPaymentsDB(t)
	pb := seedPaybill(t, db)
	tenantA, tenantB := uuid.New(), uuid.New()
	cpb := clientPaybill(tenantA, "700200")
	mustInsert(t, db.super, "rail_accounts", cpb)

	vasproImport := statementImport(pb, nil)
	clientImport := statementImport(cpb["id"].(uuid.UUID), tenantA)
	if err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		if err := insert(ctx, tx, "statement_imports", vasproImport); err != nil {
			return err
		}
		if err := insert(ctx, tx, "statement_lines", statementLine(vasproImport["id"].(uuid.UUID), "VASPRO0001")); err != nil {
			return err
		}
		return insert(ctx, tx, "recon_exceptions", reconException(pb, nil))
	}); err != nil {
		t.Fatalf("payments_system imports VASPRO's statement: %v", err)
	}
	if err := asTenant(ctx, db.app, tenantA, func(tx pgx.Tx) error {
		if err := insert(ctx, tx, "statement_imports", clientImport); err != nil {
			return err
		}
		if err := insert(ctx, tx, "statement_lines", statementLine(clientImport["id"].(uuid.UUID), "CLIENT0001")); err != nil {
			return err
		}
		return insert(ctx, tx, "recon_exceptions", reconException(cpb["id"].(uuid.UUID), tenantA))
	}); err != nil {
		t.Fatalf("a tenant imports its own statement: %v", err)
	}

	for _, table := range []string{"statement_imports", "statement_lines", "recon_exceptions"} {
		q := `SELECT count(*) FROM ` + table
		if n := tenantCount(t, db, tenantA, q); n != 1 {
			t.Errorf("%s: A sees %d, want its own 1", table, n)
		}
		if n := tenantCount(t, db, tenantB, q); n != 0 {
			t.Errorf("%s: B sees %d, want 0", table, n)
		}
		if n := roleCount(t, db, systemRole, q); n != 1 {
			t.Errorf("%s: payments_system sees %d, want VASPRO's 1", table, n)
		}
		if n := roleCount(t, db, operationsRole, q); n != 2 {
			t.Errorf("%s: operations sees %d, want 2", table, n)
		}
	}

	wantCode(t, asTenant(ctx, db.app, tenantB, func(tx pgx.Tx) error {
		return insert(ctx, tx, "statement_lines", statementLine(clientImport["id"].(uuid.UUID), "SNEAKY0001"))
	}), rlsViolation)
	wantCode(t, asRole(ctx, db.app, operationsRole, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE recon_exceptions SET status = 'resolved', resolution = 'explained', resolved_by = $1, resolved_at = now()`, uuid.New())
		if err == nil && tag.RowsAffected() != 2 {
			t.Fatalf("operations resolved %d, want 2", tag.RowsAffected())
		}
		return err
	}), "")
	wantCode(t, asRole(ctx, db.app, operationsRole, func(tx pgx.Tx) error {
		return insert(ctx, tx, "recon_exceptions", reconException(pb, nil))
	}), rlsViolation)
}

func TestRLS_STKCallbacks(t *testing.T) {
	ctx := context.Background()
	db := newPaymentsDB(t)
	pb := seedPaybill(t, db)
	if err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		return insert(ctx, tx, "stk_callbacks", stkCallback(pb, "ws_CO_1"))
	}); err != nil {
		t.Fatalf("payments_system parks a callback: %v", err)
	}
	if n := roleCount(t, db, systemRole, `SELECT count(*) FROM stk_callbacks`); n != 1 {
		t.Fatalf("payments_system sees %d", n)
	}
	tenant := uuid.New()
	if n := tenantCount(t, db, tenant, `SELECT count(*) FROM stk_callbacks`); n != 0 {
		t.Fatalf("a tenant sees %d callbacks", n)
	}
	if n := count(ctx, db.app, `SELECT count(*) FROM stk_callbacks`); n != 0 {
		t.Fatalf("the service outside WithRoleTx sees %d callbacks", n)
	}
	wantCode(t, asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		return insert(ctx, tx, "stk_callbacks", stkCallback(pb, "ws_CO_2"))
	}), rlsViolation)
}

// TestRoles_OnlyInsideWithRoleTx: the narrow roles' rows stay hidden
// from the service's ordinary connection (WITH INHERIT FALSE).
func TestRoles_OnlyInsideWithRoleTx(t *testing.T) {
	ctx := context.Background()
	db := newPaymentsDB(t)
	pb := seedPaybill(t, db)
	mustInsert(t, db.super, "collection_notices", suspenseNotice(pb, "SUSPENSE01"))
	mustInsert(t, db.super, "rail_credentials", credential(pb, nil))
	mustInsert(t, db.super, "payment_intents", intent(uuid.New(), pb))
	for _, table := range []string{"collection_notices", "rail_credentials", "payment_intents", "stk_callbacks", "recon_exceptions"} {
		if n := count(ctx, db.app, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s: the service sees %d rows outside a transaction", table, n)
		}
	}
	if n := count(ctx, db.app, `SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid
		JOIN pg_roles g ON g.oid = m.member
		WHERE r.rolname IN ('payments_system', 'payments_operations') AND g.rolname = 'payments_service' AND NOT m.inherit_option`); n != 2 {
		t.Fatalf("payments_service holds %d of the narrow roles without inheritance, want 2", n)
	}
}

// TestGrants: recorded money and receipts can't be rewritten, and
// nothing is deleted.
func TestGrants(t *testing.T) {
	ctx := context.Background()
	db := newPaymentsDB(t)
	pb := seedPaybill(t, db)
	tenant := uuid.New()
	in := intent(tenant, pb)
	mustInsert(t, db.super, "payment_intents", in)
	mustInsert(t, db.super, "collection_notices", matchedNotice(pb, tenant, "MATCHED001"))
	mustInsert(t, db.super, "collection_notices", suspenseNotice(pb, "SUSPENSE01"))
	mustInsert(t, db.super, "topup_accounts", topupAccount(tenant, pb, "ABC23456"))
	cpb := clientPaybill(tenant, "700200")
	mustInsert(t, db.super, "rail_accounts", cpb)
	mustInsert(t, db.super, "rail_credentials", credential(cpb["id"].(uuid.UUID), tenant))

	inTenant := func(sql string, args ...any) error {
		return asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err })
	}
	for name, tc := range map[string]struct {
		sql  string
		code string
	}{
		"an intent's status moves":              {`UPDATE payment_intents SET status = 'cancelled', error_code = 'PAY-6503', completed_at = now()`, ""},
		"an intent's amount is fixed":           {`UPDATE payment_intents SET amount_minor = 1`, rlsViolation},
		"an intent's tenant is fixed":           {`UPDATE payment_intents SET tenant_id = gen_random_uuid()`, rlsViolation},
		"an intent is never deleted":            {`DELETE FROM payment_intents`, rlsViolation},
		"a notice's ledger posting is recorded": {`UPDATE collection_notices SET ledger_transaction_id = gen_random_uuid()`, ""},
		"a notice's amount is fixed":            {`UPDATE collection_notices SET amount_minor = 1`, rlsViolation},
		"a notice's receipt is fixed":           {`UPDATE collection_notices SET rail_receipt = 'OTHER'`, rlsViolation},
		"a notice is never deleted":             {`DELETE FROM collection_notices`, rlsViolation},
		"an account number is permanent":        {`UPDATE topup_accounts SET account_number = 'XYZ23456'`, rlsViolation},
		"a credential is retired":               {`UPDATE rail_credentials SET status = 'retired', retired_at = now()`, ""},
		"a credential's ciphertext is fixed":    {`UPDATE rail_credentials SET ciphertext = '\x00'`, rlsViolation},
		"a credential is never deleted":         {`DELETE FROM rail_credentials`, rlsViolation},
		"an idempotency key may expire":         {`DELETE FROM idempotency_keys`, ""},
	} {
		t.Run(name, func(t *testing.T) { wantCode(t, inTenant(tc.sql), tc.code) })
	}
	t.Run("payments_system can't rewrite a suspense amount either", func(t *testing.T) {
		wantCode(t, asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE collection_notices SET amount_minor = 1`)
			return err
		}), rlsViolation)
	})
}
