//go:build integration

package store_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// newIdentityPool starts a real Postgres container, applies
// migrations/identity from empty (this alone catches a typo in the
// hand-copied schema), and returns a superuser pool with search_path
// covering both the identity schema and public — citext's operators
// live wherever CREATE EXTENSION citext ran (public, for the migration
// connection), so dropping public from search_path doesn't error, it
// just silently stops being case-insensitive.
func newIdentityPool(t *testing.T) *vasdb.Pool {
	t.Helper()
	ctx := context.Background()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/identity"))

	pool, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=identity,public")
	if err != nil {
		t.Fatalf("failed to connect with search_path=identity,public: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCreateUser_GetUserByEmail_RoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newIdentityPool(t)
	q := store.New(pool)

	created, err := q.CreateUser(ctx, store.CreateUserParams{
		ID:       uuid.New(),
		Email:    "Foo@Example.com",
		FullName: "Foo Bar",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// citext: case-insensitive lookup, not a byte-exact match.
	got, err := q.GetUserByEmail(ctx, "foo@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("got user %s, want %s", got.ID, created.ID)
	}
	if got.FullName != "Foo Bar" {
		t.Fatalf("got full_name %q, want %q", got.FullName, "Foo Bar")
	}
}

func TestCreateSession_GetSessionByID_RoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newIdentityPool(t)
	q := store.New(pool)

	user, err := q.CreateUser(ctx, store.CreateUserParams{
		ID:       uuid.New(),
		Email:    "session-owner@example.com",
		FullName: "Session Owner",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	created, err := q.CreateSession(ctx, store.CreateSessionParams{
		ID:        uuid.New(),
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := q.GetSessionByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSessionByID: %v", err)
	}
	if got.UserID != user.ID {
		t.Fatalf("got session for user %s, want %s", got.UserID, user.ID)
	}
}

func TestCreateSigningKey_ListActiveSigningKeys(t *testing.T) {
	ctx := context.Background()
	pool := newIdentityPool(t)
	q := store.New(pool)

	if _, err := q.CreateSigningKey(ctx, store.CreateSigningKeyParams{
		Kid:        "kid-1",
		Algorithm:  "EdDSA",
		PublicKey:  []byte("pub-1"),
		PrivateRef: "kms://key-1",
		Status:     "active",
	}); err != nil {
		t.Fatalf("CreateSigningKey(kid-1): %v", err)
	}
	if _, err := q.CreateSigningKey(ctx, store.CreateSigningKeyParams{
		Kid:        "kid-2-retired",
		Algorithm:  "EdDSA",
		PublicKey:  []byte("pub-2"),
		PrivateRef: "kms://key-2",
		Status:     "retired",
	}); err != nil {
		t.Fatalf("CreateSigningKey(kid-2-retired): %v", err)
	}

	active, err := q.ListActiveSigningKeys(ctx)
	if err != nil {
		t.Fatalf("ListActiveSigningKeys: %v", err)
	}
	if len(active) != 1 || active[0].Kid != "kid-1" {
		t.Fatalf("got %v, want exactly [kid-1]", active)
	}
}

// TestAssertRowLevelSecurity_OnAPIClients is the first time
// testkit.AssertRowLevelSecurity (0.12) runs against a real service
// schema instead of a scratch table.
func TestAssertRowLevelSecurity_OnAPIClients(t *testing.T) {
	ctx := context.Background()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/identity"))

	const setup = `
		CREATE ROLE identity_role LOGIN PASSWORD 'identity_role';
		GRANT USAGE ON SCHEMA identity TO identity_role;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO identity_role;
	`
	if _, err := pg.Pool.Exec(ctx, setup); err != nil {
		t.Fatalf("failed to set up identity_role: %v", err)
	}

	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("identity_role", "identity_role")+"&search_path=identity,public")
	if err != nil {
		t.Fatalf("failed to connect as identity_role: %v", err)
	}
	defer pool.Close()

	createdBy := uuid.New()
	insert := func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
		_, err := store.New(tx).CreateApiClient(ctx, store.CreateApiClientParams{
			ID:          uuid.New(),
			TenantID:    tenantID,
			Name:        "test client",
			Environment: "sandbox",
			Scopes:      []string{"sms:send"},
			IpAllowlist: []netip.Prefix{},
			CreatedBy:   createdBy,
		})
		return err
	}
	count := func(ctx context.Context, tx pgx.Tx) (int, error) {
		var n int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM identity.api_clients").Scan(&n)
		return n, err
	}

	testkit.AssertRowLevelSecurity(t, pool, insert, count)
}
