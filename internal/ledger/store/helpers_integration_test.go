//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

const migrationsDir = "../../../migrations/ledger"

// ledgerDB is one migrated Postgres with two pools: super (the
// migration-running superuser and table owner, which bypasses RLS but
// not triggers) and app (ledger_app, a login role whose only privileges
// come from the ledger_service role, as the service will connect in
// production). RLS and grants apply to app.
type ledgerDB struct {
	pg    *testkit.Postgres
	super *vasdb.Pool
	app   *vasdb.Pool
}

func newLedgerDB(t *testing.T) ledgerDB {
	t.Helper()
	ctx := context.Background()

	pg := testkit.StartPostgres(t, testkit.WithMigrations(migrationsDir))

	const setup = `
		CREATE ROLE ledger_app LOGIN PASSWORD 'ledger_app';
		GRANT ledger_service TO ledger_app;
	`
	if _, err := pg.Pool.Exec(ctx, setup); err != nil {
		t.Fatalf("failed to set up ledger_app: %v", err)
	}

	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=ledger,public")
	if err != nil {
		t.Fatalf("failed to connect as superuser: %v", err)
	}
	t.Cleanup(super.Close)

	app, err := vasdb.NewPool(ctx, pg.RoleDSN("ledger_app", "ledger_app")+"&search_path=ledger,public")
	if err != nil {
		t.Fatalf("failed to connect as ledger_app: %v", err)
	}
	t.Cleanup(app.Close)

	return ledgerDB{pg: pg, super: super, app: app}
}

const systemRole = "ledger_system"

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

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLSTATE %s, got success", code)
	}
	if got := pgCode(err); got != code {
		t.Fatalf("expected SQLSTATE %s, got %q: %v", code, got, err)
	}
}

func count(ctx context.Context, q querier, query string, args ...any) (int, error) {
	var n int
	err := q.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}

// account describes a ledger account to create. tenant nil means one of
// VASPRO's own accounts.
type account struct {
	tenant   *uuid.UUID
	typ      string
	service  *string
	rail     *string
	currency string
}

func ptr[T any](v T) *T { return &v }

func insertAccount(ctx context.Context, q querier, a account) (uuid.UUID, error) {
	id := uuid.New()
	cur := a.currency
	if cur == "" {
		cur = "KES"
	}
	_, err := q.Exec(ctx, `
		INSERT INTO accounts (id, tenant_id, type, service_code, rail, currency)
		VALUES ($1, $2, $3, $4, $5, $6)`, id, a.tenant, a.typ, a.service, a.rail, cur)
	return id, err
}

func mustAccount(t *testing.T, q querier, a account) uuid.UUID {
	t.Helper()
	id, err := insertAccount(context.Background(), q, a)
	if err != nil {
		t.Fatalf("insert %s account: %v", a.typ, err)
	}
	return id
}

// line is one journal line of a posting.
type line struct {
	account  uuid.UUID
	dir      string
	amount   int64
	currency string
}

func debit(acct uuid.UUID, amount int64) line  { return line{acct, "D", amount, "KES"} }
func credit(acct uuid.UUID, amount int64) line { return line{acct, "C", amount, "KES"} }

// post writes a transaction and its lines in the caller's database
// transaction. The 3.2 guards check balance at commit.
func post(ctx context.Context, q querier, tenant *uuid.UUID, lines ...line) (uuid.UUID, error) {
	id := uuid.New()
	if _, err := q.Exec(ctx, `
		INSERT INTO transactions (id, tenant_id, type, idempotency_key, description)
		VALUES ($1, $2, 'adjustment', $3, 'test posting')`, id, tenant, uuid.NewString()); err != nil {
		return id, err
	}
	for _, l := range lines {
		if err := insertEntry(ctx, q, id, l); err != nil {
			return id, err
		}
	}
	return id, nil
}

func insertEntry(ctx context.Context, q querier, txn uuid.UUID, l line) error {
	_, err := q.Exec(ctx, `
		INSERT INTO entries (transaction_id, account_id, direction, amount_minor, currency)
		VALUES ($1, $2, $3, $4, $5)`, txn, l.account, l.dir, l.amount, l.currency)
	return err
}

// asTenant runs fn in tenant's transaction on pool.
func asTenant(ctx context.Context, pool *vasdb.Pool, tenant uuid.UUID, fn func(tx pgx.Tx) error) error {
	return pool.WithTenantTx(ctx, tenant, func(_ context.Context, tx pgx.Tx) error { return fn(tx) })
}

// asSystem runs fn as ledger_system on pool.
func asSystem(ctx context.Context, pool *vasdb.Pool, fn func(tx pgx.Tx) error) error {
	return pool.WithRoleTx(ctx, systemRole, func(_ context.Context, tx pgx.Tx) error { return fn(tx) })
}

// inSuperTx runs fn in a superuser transaction (RLS bypassed, triggers
// not).
func inSuperTx(ctx context.Context, db ledgerDB, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, db.super, fn)
}

// fundedServiceWallet creates, in tenant's transaction, a receivable and
// an SMS service wallet and posts amount from one to the other, so the
// wallet holds money without going through a main wallet. Returns the
// service wallet and the posting.
func fundedServiceWallet(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, amount int64) (wallet, txn uuid.UUID, err error) {
	recv, err := insertAccount(ctx, tx, account{tenant: &tenant, typ: "receivable"})
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	wallet, err = insertAccount(ctx, tx, account{tenant: &tenant, typ: "service_wallet", service: ptr("sms")})
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	txn, err = post(ctx, tx, &tenant, debit(recv, amount), credit(wallet, amount))
	return wallet, txn, err
}

// listPrice inserts an approved KES SMS list price (tenant NULL) as the
// superuser and returns its id.
func listPrice(t *testing.T, db ledgerDB, minTokens string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.super.Exec(context.Background(), `
		INSERT INTO token_prices (id, tenant_id, service_code, currency, min_tokens, minor_per_token, status, effective_from, created_by, approved_by)
		VALUES ($1, NULL, 'sms', 'KES', $2, 80, 'active', '2026-01-01', $3, $4)`,
		id, minTokens, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("insert list price: %v", err)
	}
	return id
}

// ensureBalanceRows creates any missing balance rows for the given
// tenant's accounts, as the superuser. 3.2's trigger creates them on
// account insert; this keeps the 3.1 tests independent of it.
func ensureBalanceRows(t *testing.T, db ledgerDB, tenant uuid.UUID) {
	t.Helper()
	if _, err := db.super.Exec(context.Background(), `
		INSERT INTO balances (account_id) SELECT id FROM accounts WHERE tenant_id = $1
		ON CONFLICT (account_id) DO NOTHING`, tenant); err != nil {
		t.Fatalf("ensure balance rows: %v", err)
	}
}
