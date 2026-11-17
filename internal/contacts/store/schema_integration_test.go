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

// TestMigration_DownThenUp: Down really undoes Up, schema, types and roles.
func TestMigration_DownThenUp(t *testing.T) {
	ctx := context.Background()
	db := newContactsDB(t)
	if _, err := db.super.Exec(ctx, `DROP ROLE contacts_app`); err != nil {
		t.Fatalf("drop contacts_app: %v", err)
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
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'contacts'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_roles WHERE rolname IN ('contacts_service', 'contacts_system')`).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if schema != 0 || roles != 0 {
		t.Fatalf("after DownTo(0): %d contacts schema(s), %d contacts role(s) remain", schema, roles)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

// TestRLS_StandardTables: every client table carries the standard
// tenant policy, checked the canonical way.
func TestRLS_StandardTables(t *testing.T) {
	db := newContactsDB(t)
	parents := map[uuid.UUID][3]uuid.UUID{} // tenant → contact, group, import
	parent := func(t *testing.T, tenant uuid.UUID) [3]uuid.UUID {
		if p, ok := parents[tenant]; ok {
			return p
		}
		p := [3]uuid.UUID{
			seed(t, db, "contacts", contact(tenant, 900)),
			seed(t, db, "groups", group(tenant, "seeded")),
			seed(t, db, "imports", importRow(tenant)),
		}
		parents[tenant] = p
		return p
	}
	n := 0
	next := func() int { n++; return n }
	cases := map[string]struct {
		make  func(t *testing.T, tenant uuid.UUID) row
		where string
	}{
		"contacts": {func(_ *testing.T, tenant uuid.UUID) row {
			return with(contact(tenant, next()), row{"first_name": "RLS"})
		}, `first_name = 'RLS'`},
		"attribute_definitions": {func(_ *testing.T, tenant uuid.UUID) row { return attributeDef(tenant, "county") }, `true`},
		"groups":                {func(_ *testing.T, tenant uuid.UUID) row { return group(tenant, "rls") }, `name = 'rls'`},
		"group_members": {func(t *testing.T, tenant uuid.UUID) row {
			p := parent(t, tenant)
			return member(tenant, p[1], p[0])
		}, `true`},
		"segments":         {func(_ *testing.T, tenant uuid.UUID) row { return segment(tenant, "rls") }, `true`},
		"consents":         {func(_ *testing.T, tenant uuid.UUID) row { return consent(tenant, next()) }, `true`},
		"consent_events":   {func(_ *testing.T, tenant uuid.UUID) row { return consentEvent(tenant, next()) }, `true`},
		"suppressions":     {func(_ *testing.T, tenant uuid.UUID) row { return suppression(tenant, next()) }, `true`},
		"preference_links": {func(_ *testing.T, tenant uuid.UUID) row { return preferenceLink(tenant, next()) }, `true`},
		"imports":          {func(_ *testing.T, tenant uuid.UUID) row { return with(importRow(tenant), row{"file_name": "rls.csv"}) }, `file_name = 'rls.csv'`},
		"import_errors": {func(t *testing.T, tenant uuid.UUID) row {
			return importError(parent(t, tenant)[2], tenant, next())
		}, `true`},
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

// TestKeys_MembersShareTheirGroupAndContactsTenant: a foreign key check
// ignores RLS, so group_members references (id, tenant_id): a client
// that knows another client's group or contact ID can't link to it.
func TestKeys_MembersShareTheirGroupAndContactsTenant(t *testing.T) {
	ctx := context.Background()
	db := newContactsDB(t)
	owner, intruder := uuid.New(), uuid.New()
	ownerContact := seed(t, db, "contacts", contact(owner, 1))
	ownerGroup := seed(t, db, "groups", group(owner, "owner's"))
	intruderContact := seed(t, db, "contacts", contact(intruder, 2))
	intruderGroup := seed(t, db, "groups", group(intruder, "intruder's"))

	for name, r := range map[string]row{
		"the intruder's contact into the owner's group": member(intruder, ownerGroup, intruderContact),
		"the owner's contact into the intruder's group": member(intruder, intruderGroup, ownerContact),
	} {
		t.Run(name, func(t *testing.T) {
			err := asTenant(ctx, db.app, intruder, func(tx pgx.Tx) error { return insert(ctx, tx, "group_members", r) })
			wantCode(t, err, fkViolation)
		})
	}

	// Deleting a contact or a group removes its memberships.
	mustInsert(t, db.super, "group_members", member(owner, ownerGroup, ownerContact))
	wantCode(t, asTenant(ctx, db.app, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM contacts WHERE id = $1`, ownerContact)
		return err
	}), "")
	if n := count(ctx, db.super, `SELECT count(*) FROM group_members`); n != 0 {
		t.Fatalf("%d memberships survived their contact", n)
	}
}

// TestSystemRole: contacts_system claims imports and sweeps files across
// clients and finds a preference link by its hash, but sees no contact,
// consent or other client data, and only inside WithRoleTx.
func TestSystemRole(t *testing.T) {
	ctx := context.Background()
	db := newContactsDB(t)
	a, b := uuid.New(), uuid.New()
	ia := seed(t, db, "imports", finishedImport(a))
	seed(t, db, "imports", importRow(b))
	link := preferenceLink(b, 7)
	mustInsert(t, db.super, "preference_links", link)
	seed(t, db, "contacts", contact(a, 1))
	mustInsert(t, db.super, "consents", consent(a, 1))

	if n := roleCount(t, db, systemRole, `SELECT count(*) FROM imports`); n != 2 {
		t.Errorf("contacts_system sees %d imports, want both clients' 2", n)
	}
	if n := roleCount(t, db, systemRole, `SELECT count(*) FROM preference_links WHERE token_hash = $1`, link["token_hash"]); n != 1 {
		t.Errorf("contacts_system can't find a preference link by its hash")
	}
	for _, table := range []string{"contacts", "consents", "consent_events", "suppressions", "groups", "group_members", "segments", "import_errors"} {
		err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
			var n int
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		})
		if pgCode(err) != rlsViolation {
			t.Errorf("contacts_system reading %s: %v, want permission denied", table, err)
		}
	}
	wantCode(t, asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE imports SET deleted_at = now() WHERE id = $1`, ia)
		return err
	}), "")
	wantCode(t, asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE imports SET file_name = 'x' WHERE id = $1`, ia)
		return err
	}), rlsViolation)

	// Outside WithRoleTx the role is not in effect.
	if n := count(ctx, db.app, `SELECT count(*) FROM imports`); n != 0 {
		t.Errorf("contacts_app sees %d imports with no tenant and no role", n)
	}
	var inherit bool
	if err := db.super.QueryRow(ctx, `
		SELECT m.inherit_option FROM pg_auth_members m
		JOIN pg_roles r ON r.oid = m.roleid JOIN pg_roles g ON g.oid = m.member
		WHERE r.rolname = 'contacts_system' AND g.rolname = 'contacts_service'`).Scan(&inherit); err != nil {
		t.Fatal(err)
	}
	if inherit {
		t.Error("contacts_system is granted to contacts_service with inheritance")
	}
}

// TestGrants: column-limited updates, inserts only on the histories,
// deletes only where the design allows them.
func TestGrants(t *testing.T) {
	ctx := context.Background()
	db := newContactsDB(t)
	tenant := uuid.New()
	c := seed(t, db, "contacts", contact(tenant, 1))
	g := seed(t, db, "groups", group(tenant, "g"))
	imp := seed(t, db, "imports", importRow(tenant))
	mustInsert(t, db.super, "consents", consent(tenant, 1))
	mustInsert(t, db.super, "consent_events", consentEvent(tenant, 1))
	mustInsert(t, db.super, "import_errors", importError(imp, tenant, 3))
	mustInsert(t, db.super, "suppressions", suppression(tenant, 1))
	mustInsert(t, db.super, "group_members", member(tenant, g, c))

	for _, tc := range []struct {
		name string
		sql  string
		code string
	}{
		{"a contact's name changes", `UPDATE contacts SET first_name = 'Changed', version = version + 1`, ""},
		{"a contact's number doesn't", `UPDATE contacts SET phone_e164 = '+999001234567'`, rlsViolation},
		{"a contact's tenant doesn't", `UPDATE contacts SET tenant_id = gen_random_uuid()`, rlsViolation},
		{"a consent's status changes", `UPDATE consents SET status = 'withdrawn'`, ""},
		{"a consent's number doesn't", `UPDATE consents SET phone_e164 = '+999001234567'`, rlsViolation},
		{"consent history isn't rewritten", `UPDATE consent_events SET status = 'withdrawn'`, rlsViolation},
		{"consent history isn't deleted", `DELETE FROM consent_events`, rlsViolation},
		{"consents aren't deleted", `DELETE FROM consents`, rlsViolation},
		{"an import's status changes", `UPDATE imports SET status = 'scanning'`, ""},
		{"an import's object key doesn't", `UPDATE imports SET object_key = 'elsewhere'`, rlsViolation},
		{"an import's file name doesn't", `UPDATE imports SET file_name = 'other.csv'`, rlsViolation},
		{"import errors aren't rewritten", `UPDATE import_errors SET message = 'x'`, rlsViolation},
		{"import errors aren't deleted", `DELETE FROM import_errors`, rlsViolation},
		{"imports aren't deleted", `DELETE FROM imports`, rlsViolation},
		{"a suppression is deleted", `DELETE FROM suppressions`, ""},
		{"a membership is deleted", `DELETE FROM group_members`, ""},
		{"a group is deleted", `DELETE FROM groups`, ""},
		{"a contact is deleted", `DELETE FROM contacts`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantCode(t, asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, tc.sql)
				return err
			}), tc.code)
		})
	}
}

// TestTrigger_SubjectWithdrawalSticks: a withdrawal the person made can
// only be undone by the person (data model §10).
func TestTrigger_SubjectWithdrawalSticks(t *testing.T) {
	ctx := context.Background()
	db := newContactsDB(t)
	tenant := uuid.New()
	mustInsert(t, db.super, "consents", subjectWithdrawal(tenant, 1))
	update := func(set string) error {
		return asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE consents SET `+set+` WHERE phone_e164 = $1`, phone(1))
			return err
		})
	}
	upsert := func(r row) error {
		return asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO consents (tenant_id, phone_e164, channel, purpose, status, actor, source, recorded_by, recorded_at)
				VALUES ($1, $2, 'sms', 'promotional', $3, $4, $5, $6, now())
				ON CONFLICT (tenant_id, phone_e164, channel, purpose)
				DO UPDATE SET status = excluded.status, actor = excluded.actor, source = excluded.source,
				              recorded_by = excluded.recorded_by, recorded_at = excluded.recorded_at`,
				tenant, phone(1), r["status"], r["actor"], r["source"], r["recorded_by"])
			return err
		})
	}
	wantCode(t, update(`status = 'granted', actor = 'client', source = 'api', recorded_by = gen_random_uuid()`), checkViolation)
	wantCode(t, upsert(row{"status": "granted", "actor": "client", "source": "api", "recorded_by": uuid.New()}), checkViolation)
	wantCode(t, upsert(row{"status": "withdrawn", "actor": "client", "source": "api", "recorded_by": uuid.New()}), checkViolation)
	// The person can give it back, and then the client may change it.
	wantCode(t, upsert(row{"status": "granted", "actor": "subject", "source": "preference_centre", "recorded_by": nil}), "")
	wantCode(t, upsert(row{"status": "withdrawn", "actor": "client", "source": "api", "recorded_by": uuid.New()}), "")
	wantCode(t, upsert(row{"status": "granted", "actor": "client", "source": "api", "recorded_by": uuid.New()}), "")

	// A client's own withdrawal is the client's to undo.
	mustInsert(t, db.super, "consents", with(consent(tenant, 2), row{"status": "withdrawn"}))
	wantCode(t, asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE consents SET status = 'granted' WHERE phone_e164 = $1`, phone(2))
		return err
	}), "")
}
