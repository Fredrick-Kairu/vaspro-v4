//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

const migrationsDir = "../../../migrations/payments"

const (
	systemRole     = "payments_system"
	operationsRole = "payments_operations"
)

// paymentsDB is one migrated Postgres with two pools: super (the
// migration-running superuser and table owner, which bypasses RLS but
// not CHECKs) and app (payments_app, a login role whose only privileges
// come from payments_service, as the service will connect). RLS and
// grants apply to app.
type paymentsDB struct {
	pg    *testkit.Postgres
	super *vasdb.Pool
	app   *vasdb.Pool
}

func newPaymentsDB(t *testing.T) paymentsDB {
	t.Helper()
	ctx := context.Background()

	pg := testkit.StartPostgres(t, testkit.WithMigrations(migrationsDir))
	if _, err := pg.Pool.Exec(ctx, `
		CREATE ROLE payments_app LOGIN PASSWORD 'payments_app';
		GRANT payments_service TO payments_app;
	`); err != nil {
		t.Fatalf("failed to set up payments_app: %v", err)
	}

	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=payments,public")
	if err != nil {
		t.Fatalf("failed to connect as superuser: %v", err)
	}
	t.Cleanup(super.Close)
	app, err := vasdb.NewPool(ctx, pg.RoleDSN("payments_app", "payments_app")+"&search_path=payments,public")
	if err != nil {
		t.Fatalf("failed to connect as payments_app: %v", err)
	}
	t.Cleanup(app.Close)
	return paymentsDB{pg: pg, super: super, app: app}
}

// querier is satisfied by both a pool and a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// wantCode checks err has SQLSTATE code; "" means success.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if code == "" {
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected SQLSTATE %s, got success", code)
	}
	if got := pgCode(err); got != code {
		t.Fatalf("expected SQLSTATE %s, got %q: %v", code, got, err)
	}
}

func count(ctx context.Context, q querier, query string, args ...any) int {
	var n int
	if err := q.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		panic(fmt.Sprintf("count %q: %v", query, err))
	}
	return n
}

// row is one row to insert, by column. Test-only: the table and column
// names are this file's constants, never input.
type row map[string]any

// drop removes a column from a row in with.
var drop = &struct{}{}

// with copies base and applies changes; a change to drop removes it.
func with(base row, changes row) row {
	r := maps.Clone(base)
	for k, v := range changes {
		if v == drop {
			delete(r, k)
			continue
		}
		r[k] = v
	}
	return r
}

func insert(ctx context.Context, q querier, table string, r row) error {
	cols := slices.Sorted(maps.Keys(r))
	ph := make([]string, len(cols))
	args := make([]any, len(cols))
	for i, c := range cols {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = r[c]
	}
	_, err := q.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(cols, ", "), strings.Join(ph, ", ")), args...)
	return err
}

func mustInsert(t *testing.T, q querier, table string, r row) {
	t.Helper()
	if err := insert(context.Background(), q, table, r); err != nil {
		t.Fatalf("insert into %s: %v", table, err)
	}
}

func asTenant(ctx context.Context, pool *vasdb.Pool, tenant uuid.UUID, fn func(tx pgx.Tx) error) error {
	return pool.WithTenantTx(ctx, tenant, func(_ context.Context, tx pgx.Tx) error { return fn(tx) })
}

func asRole(ctx context.Context, pool *vasdb.Pool, role string, fn func(tx pgx.Tx) error) error {
	return pool.WithRoleTx(ctx, role, func(_ context.Context, tx pgx.Tx) error { return fn(tx) })
}

// tenantCount counts rows visible to tenant.
func tenantCount(t *testing.T, db paymentsDB, tenant uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	if err := asTenant(context.Background(), db.app, tenant, func(tx pgx.Tx) error {
		n = count(context.Background(), tx, query, args...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// roleCount counts rows visible to a narrow role.
func roleCount(t *testing.T, db paymentsDB, role, query string, args ...any) int {
	t.Helper()
	var n int
	if err := asRole(context.Background(), db.app, role, func(tx pgx.Tx) error {
		n = count(context.Background(), tx, query, args...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// Fixtures. Phone numbers are Safaricom's published sandbox number.

const sandboxPhone = "+254708374149"

var now = time.Now().UTC().Truncate(time.Second)

func mpesaRail() row {
	return row{"code": "mpesa_ke", "country_code": "KE", "kind": "mobile_money", "capabilities": []string{"stk", "c2b", "status_query"}, "status": "live"}
}

// vasproPaybill is VASPRO's own top-up paybill (tenant NULL).
func vasproPaybill() row {
	return row{
		"id": uuid.New(), "tenant_id": nil, "rail_code": "mpesa_ke", "account_type": "paybill",
		"number": "600100", "display_name": "VASPRO", "uses": []string{"vaspro_topups"},
		"status": "active", "verified_at": now,
	}
}

// clientPaybill is a client's own collections paybill.
func clientPaybill(tenant uuid.UUID, number string) row {
	return row{
		"id": uuid.New(), "tenant_id": tenant, "rail_code": "mpesa_ke", "account_type": "paybill",
		"number": number, "display_name": "Client Ltd", "uses": []string{"collections"},
		"status": "pending_verification",
	}
}

func credential(railAccount uuid.UUID, tenant any) row {
	return row{
		"id": uuid.New(), "rail_account_id": railAccount, "tenant_id": tenant, "kind": "app_key",
		"ciphertext": []byte{1, 2, 3}, "kms_key_id": "payments-rail-credentials", "fingerprint": []byte{4, 5, 6},
		"status": "active", "created_by": uuid.New(),
	}
}

func intent(tenant, railAccount uuid.UUID) row {
	return row{
		"id": uuid.New(), "tenant_id": tenant, "purpose": "wallet_topup", "rail_account_id": railAccount,
		"method": "stk_push", "amount_minor": int64(150000), "currency": "KES", "payer_phone_e164": sandboxPhone,
		"account_reference": "ABC23456", "status": "pending", "idempotency_key": uuid.NewString(),
		"expires_at": now.Add(2 * time.Minute),
	}
}

// suspenseNotice is an unmatched payment to VASPRO's paybill.
func suspenseNotice(railAccount uuid.UUID, receipt string) row {
	return row{
		"id": uuid.New(), "rail_account_id": railAccount, "tenant_id": nil, "rail_receipt": receipt,
		"transaction_type": "PayBill", "amount_minor": int64(100000), "currency": "KES", "bill_ref": "ZZZZZZZZ",
		"payer_phone_e164": sandboxPhone, "paid_at": now, "match_status": "suspense", "raw": `{}`,
	}
}

func matchedNotice(railAccount, tenant uuid.UUID, receipt string) row {
	return with(suspenseNotice(railAccount, receipt), row{"match_status": "matched", "matched_tenant_id": tenant, "bill_ref": "ABC23456"})
}

func statementImport(railAccount uuid.UUID, tenant any) row {
	return row{
		"id": uuid.New(), "rail_account_id": railAccount, "tenant_id": tenant,
		"period_start": now.Add(-24 * time.Hour), "period_end": now, "source": "file", "status": "importing",
	}
}

func statementLine(importID uuid.UUID, receipt string) row {
	return row{"import_id": importID, "rail_receipt": receipt, "direction": "I", "amount_minor": int64(100000), "occurred_at": now, "match_status": "unmatched"}
}

func reconException(railAccount uuid.UUID, tenant any) row {
	return row{"id": uuid.New(), "tenant_id": tenant, "rail_account_id": railAccount, "kind": "missing_in_vaspro", "status": "open"}
}

func topupAccount(tenant, railAccount uuid.UUID, number string) row {
	return row{"id": uuid.New(), "tenant_id": tenant, "rail_account_id": railAccount, "account_number": number}
}

func stkCallback(railAccount uuid.UUID, checkout string) row {
	return row{"id": uuid.New(), "rail_account_id": railAccount, "checkout_request_id": checkout, "result_code": "0", "raw": `{}`}
}

// seedPaybill inserts the M-Pesa rail and VASPRO's paybill as the owner.
func seedPaybill(t *testing.T, db paymentsDB) uuid.UUID {
	t.Helper()
	mustInsert(t, db.super, "rails", mpesaRail())
	pb := vasproPaybill()
	mustInsert(t, db.super, "rail_accounts", pb)
	return pb["id"].(uuid.UUID)
}
