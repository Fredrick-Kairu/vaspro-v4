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

const migrationsDir = "../../../migrations/webhooks"

const systemRole = "webhooks_system"

// webhooksDB is one migrated Postgres with two pools: super (the
// migration-running superuser and table owner, which bypasses RLS but
// not CHECKs) and app (webhooks_app, a login role whose only privileges
// come from webhooks_service, as the service will connect). RLS and
// grants apply to app.
type webhooksDB struct {
	pg    *testkit.Postgres
	super *vasdb.Pool
	app   *vasdb.Pool
}

func newWebhooksDB(t *testing.T) webhooksDB {
	t.Helper()
	ctx := context.Background()

	pg := testkit.StartPostgres(t, testkit.WithMigrations(migrationsDir))
	if _, err := pg.Pool.Exec(ctx, `
		CREATE ROLE webhooks_app LOGIN PASSWORD 'webhooks_app';
		GRANT webhooks_service TO webhooks_app;
	`); err != nil {
		t.Fatalf("failed to set up webhooks_app: %v", err)
	}

	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=webhooks,public")
	if err != nil {
		t.Fatalf("failed to connect as superuser: %v", err)
	}
	t.Cleanup(super.Close)
	app, err := vasdb.NewPool(ctx, pg.RoleDSN("webhooks_app", "webhooks_app")+"&search_path=webhooks,public")
	if err != nil {
		t.Fatalf("failed to connect as webhooks_app: %v", err)
	}
	t.Cleanup(app.Close)
	return webhooksDB{pg: pg, super: super, app: app}
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
func tenantCount(t *testing.T, db webhooksDB, tenant uuid.UUID, query string, args ...any) int {
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
func roleCount(t *testing.T, db webhooksDB, role, query string, args ...any) int {
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

func endpoint(tenant uuid.UUID) row {
	return row{
		"id": uuid.New(), "tenant_id": tenant, "url": "https://hooks.example.com/vaspro",
		"status": "pending_verification", "created_by": uuid.New(),
	}
}

func activeEndpoint(tenant uuid.UUID) row {
	return with(endpoint(tenant), row{"status": "active", "verified_at": now})
}

func secret(endpointID, tenant uuid.UUID) row {
	return row{
		"id": uuid.New(), "tenant_id": tenant, "endpoint_id": endpointID, "ciphertext": []byte{1, 2, 3},
		"kms_key_id": "webhooks-endpoint-secrets", "fingerprint": []byte{4, 5, 6}, "created_by": uuid.New(),
	}
}

func subscription(endpointID, tenant uuid.UUID, eventType string) row {
	return row{"tenant_id": tenant, "endpoint_id": endpointID, "event_type": eventType}
}

func delivery(endpointID, tenant uuid.UUID) row {
	return row{
		"id": uuid.New(), "tenant_id": tenant, "endpoint_id": endpointID, "event_id": uuid.New(),
		"event_type": "wallet.credited", "occurred_at": now, "body": []byte(`{"id":"x"}`),
		"status": "pending", "next_attempt_at": now,
	}
}

func attempt(deliveryID, tenant uuid.UUID, n int) row {
	return row{
		"id": uuid.New(), "tenant_id": tenant, "delivery_id": deliveryID, "attempt": n,
		"started_at": now, "duration_ms": 120, "request_headers": `{}`,
		"outcome": "http_status", "response_status": 500,
	}
}

// seedEndpoint inserts an active endpoint for tenant as the owner.
func seedEndpoint(t *testing.T, db webhooksDB, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	e := activeEndpoint(tenant)
	mustInsert(t, db.super, "endpoints", e)
	return e["id"].(uuid.UUID)
}

// seedDelivery inserts a pending delivery to endpointID as the owner.
func seedDelivery(t *testing.T, db webhooksDB, endpointID, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	d := delivery(endpointID, tenant)
	mustInsert(t, db.super, "deliveries", d)
	return d["id"].(uuid.UUID)
}
