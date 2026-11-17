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

const migrationsDir = "../../../migrations/contacts"

const systemRole = "contacts_system"

// contactsDB is one migrated Postgres with two pools: super (the
// migration-running superuser and table owner, which bypasses RLS but
// not CHECKs) and app (contacts_app, a login role whose only privileges
// come from contacts_service, as the service will connect). RLS and
// grants apply to app.
type contactsDB struct {
	pg    *testkit.Postgres
	super *vasdb.Pool
	app   *vasdb.Pool
}

func newContactsDB(t *testing.T) contactsDB {
	t.Helper()
	ctx := context.Background()

	pg := testkit.StartPostgres(t, testkit.WithMigrations(migrationsDir))
	if _, err := pg.Pool.Exec(ctx, `
		CREATE ROLE contacts_app LOGIN PASSWORD 'contacts_app';
		GRANT contacts_service TO contacts_app;
	`); err != nil {
		t.Fatalf("failed to set up contacts_app: %v", err)
	}

	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=contacts,public")
	if err != nil {
		t.Fatalf("failed to connect as superuser: %v", err)
	}
	t.Cleanup(super.Close)
	app, err := vasdb.NewPool(ctx, pg.RoleDSN("contacts_app", "contacts_app")+"&search_path=contacts,public")
	if err != nil {
		t.Fatalf("failed to connect as contacts_app: %v", err)
	}
	t.Cleanup(app.Close)
	return contactsDB{pg: pg, super: super, app: app}
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

// roleCount counts rows visible to a narrow role.
func roleCount(t *testing.T, db contactsDB, role, query string, args ...any) int {
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

// Fixtures.

var now = time.Now().UTC().Truncate(time.Second)

// phone is a test number under country code 999, which no country has,
// so it can never be a real person's number.
func phone(n int) string { return fmt.Sprintf("+99900%06d", n) }

func contact(tenant uuid.UUID, n int) row {
	return row{"id": uuid.New(), "tenant_id": tenant, "phone_e164": phone(n), "source": "api", "created_by": uuid.New()}
}

func group(tenant uuid.UUID, name string) row {
	return row{"id": uuid.New(), "tenant_id": tenant, "name": name}
}

func member(tenant, groupID, contactID uuid.UUID) row {
	return row{"tenant_id": tenant, "group_id": groupID, "contact_id": contactID}
}

func attributeDef(tenant uuid.UUID, key string) row {
	return row{"tenant_id": tenant, "key": key, "label": "County", "type": "text"}
}

func segment(tenant uuid.UUID, name string) row {
	return row{"id": uuid.New(), "tenant_id": tenant, "name": name, "filter": `{"all":[]}`}
}

func consent(tenant uuid.UUID, n int) row {
	return row{
		"tenant_id": tenant, "phone_e164": phone(n), "channel": "sms", "purpose": "promotional",
		"status": "granted", "actor": "client", "source": "api", "recorded_by": uuid.New(), "recorded_at": now,
	}
}

func subjectWithdrawal(tenant uuid.UUID, n int) row {
	return with(consent(tenant, n), row{"status": "withdrawn", "actor": "subject", "source": "keyword", "recorded_by": drop})
}

func consentEvent(tenant uuid.UUID, n int) row {
	return row{
		"id": uuid.New(), "tenant_id": tenant, "phone_e164": phone(n), "channel": "sms", "purpose": "promotional",
		"status": "granted", "actor": "client", "source": "api", "occurred_at": now,
	}
}

func suppression(tenant uuid.UUID, n int) row {
	return row{"tenant_id": tenant, "phone_e164": phone(n), "reason": "client_request", "created_by": uuid.New()}
}

func preferenceLink(tenant uuid.UUID, n int) row {
	return row{
		"tenant_id": tenant, "phone_e164": phone(n), "token_hash": uuid.New().NodeID(),
		"created_at": now, "expires_at": now.Add(90 * 24 * time.Hour),
	}
}

func importRow(tenant uuid.UUID) row {
	id := uuid.New()
	return row{
		"id": id, "tenant_id": tenant, "file_name": "contacts.csv", "content_type": "text/csv",
		"object_key":        "uploads/transient/" + tenant.String() + "/" + id.String(),
		"upload_expires_at": now.Add(15 * time.Minute), "delete_after": now.Add(48 * time.Hour),
		"created_by": uuid.New(),
	}
}

func finishedImport(tenant uuid.UUID) row {
	return with(importRow(tenant), row{"status": "completed", "finished_at": now, "rows_total": 10, "rows_valid": 10})
}

func importError(importID, tenant uuid.UUID, rowNo int) row {
	return row{"import_id": importID, "tenant_id": tenant, "row_no": rowNo, "field": "phone", "error_code": "VAL-4000", "message": "not a mobile number"}
}

// seed inserts r into table as the owner and returns its id.
func seed(t *testing.T, db contactsDB, table string, r row) uuid.UUID {
	t.Helper()
	mustInsert(t, db.super, table, r)
	id, _ := r["id"].(uuid.UUID)
	return id
}
