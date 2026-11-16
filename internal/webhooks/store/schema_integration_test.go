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
	db := newWebhooksDB(t)
	if _, err := db.super.Exec(ctx, `DROP ROLE webhooks_app`); err != nil {
		t.Fatalf("drop webhooks_app: %v", err)
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
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'webhooks'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_roles WHERE rolname IN ('webhooks_service', 'webhooks_system')`).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if schema != 0 || roles != 0 {
		t.Fatalf("after DownTo(0): %d webhooks schema(s), %d webhooks role(s) remain", schema, roles)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

// TestRLS_StandardTables: every client table carries the standard
// tenant policy, checked the canonical way.
func TestRLS_StandardTables(t *testing.T) {
	db := newWebhooksDB(t)
	tenantEndpoint := map[uuid.UUID]uuid.UUID{}
	tenantDelivery := map[uuid.UUID]uuid.UUID{}
	// Children need their tenant's parent row, made by the owner first.
	parent := func(t *testing.T, tenant uuid.UUID) (uuid.UUID, uuid.UUID) {
		if e, ok := tenantEndpoint[tenant]; ok {
			return e, tenantDelivery[tenant]
		}
		e := seedEndpoint(t, db, tenant)
		d := seedDelivery(t, db, e, tenant)
		tenantEndpoint[tenant], tenantDelivery[tenant] = e, d
		return e, d
	}

	// Each case tags the rows it inserts (where), so the count leaves out
	// the parents seeded by the owner, which the tenant sees too.
	cases := map[string]struct {
		make  func(t *testing.T, tenant uuid.UUID) row
		where string
	}{
		"endpoints": {
			func(_ *testing.T, tenant uuid.UUID) row {
				return with(endpoint(tenant), row{"url": "https://rls.example.com/"})
			},
			`url = 'https://rls.example.com/'`,
		},
		"endpoint_secrets": {
			func(t *testing.T, tenant uuid.UUID) row {
				e, _ := parent(t, tenant)
				return with(secret(e, tenant), row{"expires_at": now})
			},
			`expires_at IS NOT NULL`,
		},
		"subscriptions": {
			func(t *testing.T, tenant uuid.UUID) row {
				e, _ := parent(t, tenant)
				return subscription(e, tenant, "tokens.purchased")
			},
			`true`,
		},
		"deliveries": {
			func(t *testing.T, tenant uuid.UUID) row {
				e, _ := parent(t, tenant)
				return with(delivery(e, tenant), row{"event_type": "tokens.purchased"})
			},
			`event_type = 'tokens.purchased'`,
		},
		"delivery_attempts": {
			func(t *testing.T, tenant uuid.UUID) row {
				_, d := parent(t, tenant)
				return attempt(d, tenant, 1)
			},
			`true`,
		},
	}
	for table, tc := range cases {
		t.Run(table, func(t *testing.T) {
			testkit.AssertRowLevelSecurity(t, db.app,
				func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
					return insert(ctx, tx, table, tc.make(t, tenant))
				},
				func(ctx context.Context, tx pgx.Tx) (int, error) {
					return count(ctx, tx, `SELECT count(*) FROM `+table+` WHERE `+tc.where), nil
				})
		})
	}
}

// TestKeys_ChildrenShareTheirParentsTenant: a foreign key check ignores
// RLS, so the (id, tenant_id) references are what stop a client
// attaching rows to another client's endpoint or delivery, even one it
// knows the ID of.
func TestKeys_ChildrenShareTheirParentsTenant(t *testing.T) {
	ctx := context.Background()
	db := newWebhooksDB(t)
	owner, intruder := uuid.New(), uuid.New()
	e := seedEndpoint(t, db, owner)
	d := seedDelivery(t, db, e, owner)

	for name, tc := range map[string]struct {
		table string
		r     row
	}{
		"a secret on another client's endpoint":       {"endpoint_secrets", secret(e, intruder)},
		"a subscription on another client's endpoint": {"subscriptions", subscription(e, intruder, "*")},
		"a delivery to another client's endpoint":     {"deliveries", delivery(e, intruder)},
		"an attempt on another client's delivery":     {"delivery_attempts", attempt(d, intruder, 1)},
	} {
		t.Run(name, func(t *testing.T) {
			wantCode(t, asTenant(ctx, db.app, intruder, func(tx pgx.Tx) error {
				return insert(ctx, tx, tc.table, tc.r)
			}), fkViolation)
		})
	}
	t.Run("a replay of another client's delivery", func(t *testing.T) {
		ie := seedEndpoint(t, db, intruder)
		wantCode(t, asTenant(ctx, db.app, intruder, func(tx pgx.Tx) error {
			return insert(ctx, tx, "deliveries", with(delivery(ie, intruder), row{"replay_of": d, "replayed_by": uuid.New()}))
		}), fkViolation)
	})
}

// TestRLS_SystemRole: the delivery worker reads and moves every
// client's deliveries, reads endpoints and secrets, and records
// attempts, but only inside WithRoleTx.
func TestRLS_SystemRole(t *testing.T) {
	ctx := context.Background()
	db := newWebhooksDB(t)
	a, b := uuid.New(), uuid.New()
	ea, eb := seedEndpoint(t, db, a), seedEndpoint(t, db, b)
	mustInsert(t, db.super, "endpoint_secrets", secret(ea, a))
	mustInsert(t, db.super, "endpoint_secrets", secret(eb, b))
	da, db2 := seedDelivery(t, db, ea, a), seedDelivery(t, db, eb, b)

	for table, want := range map[string]int{"endpoints": 2, "endpoint_secrets": 2, "deliveries": 2} {
		if n := roleCount(t, db, systemRole, `SELECT count(*) FROM `+table); n != want {
			t.Errorf("webhooks_system sees %d %s, want %d", n, table, want)
		}
	}
	wantCode(t, asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET leased_until = now() + interval '1 minute' WHERE id IN ($1, $2)`, da, db2); err != nil {
			return err
		}
		if err := insert(ctx, tx, "delivery_attempts", attempt(da, a, 1)); err != nil {
			return err
		}
		if err := insert(ctx, tx, "delivery_attempts", attempt(db2, b, 1)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE endpoints SET status = 'disabled', disabled_at = now(), disabled_reason = 'failing', failing_since = now() - interval '73 hours' WHERE id = $1`, ea)
		return err
	}), "")
	if n := count(ctx, db.super, `SELECT count(*) FROM deliveries WHERE leased_until IS NOT NULL`); n != 2 {
		t.Fatalf("leased %d deliveries, want 2", n)
	}

	t.Run("it can't create endpoints or touch secrets", func(t *testing.T) {
		wantCode(t, asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
			return insert(ctx, tx, "endpoints", endpoint(a))
		}), rlsViolation)
		wantCode(t, asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE endpoint_secrets SET expires_at = now()`)
			return err
		}), rlsViolation)
	})
	t.Run("a tenant sees only its own", func(t *testing.T) {
		if n := tenantCount(t, db, a, `SELECT count(*) FROM deliveries`); n != 1 {
			t.Fatalf("tenant A sees %d deliveries, want 1", n)
		}
	})
}

// TestRoles_OnlyInsideWithRoleTx: the narrow role's reach stays hidden
// from the service's ordinary connection (WITH INHERIT FALSE).
func TestRoles_OnlyInsideWithRoleTx(t *testing.T) {
	ctx := context.Background()
	db := newWebhooksDB(t)
	tenant := uuid.New()
	e := seedEndpoint(t, db, tenant)
	mustInsert(t, db.super, "endpoint_secrets", secret(e, tenant))
	seedDelivery(t, db, e, tenant)
	for _, table := range []string{"endpoints", "endpoint_secrets", "deliveries"} {
		if n := count(ctx, db.app, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s: the service sees %d rows outside a transaction", table, n)
		}
	}
	if n := count(ctx, db.app, `SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid
		JOIN pg_roles g ON g.oid = m.member
		WHERE r.rolname = 'webhooks_system' AND g.rolname = 'webhooks_service' AND NOT m.inherit_option`); n != 1 {
		t.Fatalf("webhooks_service holds webhooks_system without inheritance %d times, want 1", n)
	}
}

// TestGrants: what was sent, and to whom, can't be rewritten, and
// nothing is deleted but subscriptions and idempotency keys.
func TestGrants(t *testing.T) {
	ctx := context.Background()
	db := newWebhooksDB(t)
	tenant := uuid.New()
	e := seedEndpoint(t, db, tenant)
	mustInsert(t, db.super, "endpoint_secrets", secret(e, tenant))
	mustInsert(t, db.super, "subscriptions", subscription(e, tenant, "*"))
	d := seedDelivery(t, db, e, tenant)
	mustInsert(t, db.super, "delivery_attempts", attempt(d, tenant, 1))

	inTenant := func(sql string) error {
		return asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err })
	}
	for name, tc := range map[string]struct {
		sql  string
		code string
	}{
		"an endpoint's URL changes":            {`UPDATE endpoints SET url = 'https://other.example.com/', status = 'pending_verification', verified_at = NULL`, ""},
		"an endpoint is deleted softly":        {`UPDATE endpoints SET deleted_at = now()`, ""},
		"an endpoint's tenant is fixed":        {`UPDATE endpoints SET tenant_id = gen_random_uuid()`, rlsViolation},
		"an endpoint's maker is fixed":         {`UPDATE endpoints SET created_by = gen_random_uuid()`, rlsViolation},
		"an endpoint is never deleted":         {`DELETE FROM endpoints`, rlsViolation},
		"a secret expires":                     {`UPDATE endpoint_secrets SET expires_at = now()`, ""},
		"a secret's ciphertext is fixed":       {`UPDATE endpoint_secrets SET ciphertext = '\x00'`, rlsViolation},
		"a secret is never deleted":            {`DELETE FROM endpoint_secrets`, rlsViolation},
		"a subscription may be removed":        {`DELETE FROM subscriptions`, ""},
		"a delivery is scheduled":              {`UPDATE deliveries SET attempts = 1, next_attempt_at = now() + interval '30 seconds'`, ""},
		"a delivery's body is fixed":           {`UPDATE deliveries SET body = '\x00'`, rlsViolation},
		"a delivery's event is fixed":          {`UPDATE deliveries SET event_id = gen_random_uuid()`, rlsViolation},
		"a delivery's endpoint is fixed":       {`UPDATE deliveries SET endpoint_id = gen_random_uuid()`, rlsViolation},
		"a delivery is never deleted":          {`DELETE FROM deliveries`, rlsViolation},
		"an attempt is never rewritten":        {`UPDATE delivery_attempts SET response_status = 200`, rlsViolation},
		"an attempt is never deleted":          {`DELETE FROM delivery_attempts`, rlsViolation},
		"an idempotency key may expire":        {`DELETE FROM idempotency_keys`, ""},
		"a processed event is never forgotten": {`DELETE FROM processed_events`, rlsViolation},
	} {
		t.Run(name, func(t *testing.T) { wantCode(t, inTenant(tc.sql), tc.code) })
	}
	t.Run("webhooks_system can't rewrite a body either", func(t *testing.T) {
		wantCode(t, asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE deliveries SET body = '\x00'`)
			return err
		}), rlsViolation)
	})
}
