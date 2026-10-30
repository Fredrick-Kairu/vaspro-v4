//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func mustUser(t *testing.T, q *store.Queries, email string) store.IdentityUser {
	t.Helper()
	u, err := q.CreateUser(context.Background(), store.CreateUserParams{ID: uuid.New(), Email: email, FullName: "Test User"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// TestUserIdentities_Constraints covers migration 20261030120000's
// user_identities: one link per Google account, one Google account per
// user, and only known providers.
func TestUserIdentities_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newIdentityPool(t)
	q := store.New(pool)

	alice := mustUser(t, q, "alice@example.com")
	bob := mustUser(t, q, "bob@example.com")

	link := func(user uuid.UUID, provider, subject string) error {
		_, err := q.CreateUserIdentity(ctx, store.CreateUserIdentityParams{
			ID: uuid.New(), UserID: user, Provider: provider, Subject: subject, EmailAtLink: "x@example.com",
		})
		return err
	}

	if err := link(alice.ID, "google", "sub-alice"); err != nil {
		t.Fatalf("linking alice: %v", err)
	}
	t.Run("a Google account links to only one user", func(t *testing.T) {
		if code := pgErrCode(link(bob.ID, "google", "sub-alice")); code != "23505" {
			t.Fatalf("got %q, want unique violation 23505", code)
		}
	})
	t.Run("a user links only one Google account", func(t *testing.T) {
		if code := pgErrCode(link(alice.ID, "google", "sub-other")); code != "23505" {
			t.Fatalf("got %q, want unique violation 23505", code)
		}
	})
	t.Run("unknown providers are rejected", func(t *testing.T) {
		if code := pgErrCode(link(bob.ID, "myspace", "sub-bob")); code != "23514" {
			t.Fatalf("got %q, want check violation 23514", code)
		}
	})
	t.Run("lookup is by subject, whatever the email", func(t *testing.T) {
		got, err := q.GetUserIdentityBySubject(ctx, store.GetUserIdentityBySubjectParams{Provider: "google", Subject: "sub-alice"})
		if err != nil {
			t.Fatalf("GetUserIdentityBySubject: %v", err)
		}
		if got.UserID != alice.ID {
			t.Fatalf("got user %s, want alice %s", got.UserID, alice.ID)
		}
	})
}

func TestSessions_AuthMethodCheck(t *testing.T) {
	ctx := context.Background()
	pool := newIdentityPool(t)
	q := store.New(pool)
	user := mustUser(t, q, "carol@example.com")

	create := func(method string) error {
		_, err := q.CreateSession(ctx, store.CreateSessionParams{
			ID: uuid.New(), UserID: user.ID, ExpiresAt: time.Now().Add(time.Hour),
			TokenHash: uuid.New().NodeID(), AuthMethod: method,
		})
		return err
	}
	for _, m := range []string{"password", "passkey", "google", "sso"} {
		if err := create(m); err != nil {
			t.Fatalf("auth_method %q rejected: %v", m, err)
		}
	}
	if code := pgErrCode(create("magic_link")); code != "23514" {
		t.Fatalf("got %q, want check violation 23514", code)
	}
}

// TestSSODomainLookupRole_OnlyInsideWithRoleTx mirrors 1.8's
// TestTokenExchangeRole_CanReadApiClientsAcrossTenants for the new
// identity_sso_domain_lookup role.
func TestSSODomainLookupRole_OnlyInsideWithRoleTx(t *testing.T) {
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/identity"))

	const setup = `
		CREATE ROLE identity_role LOGIN PASSWORD 'identity_role';
		GRANT USAGE ON SCHEMA identity TO identity_role;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO identity_role;
		GRANT identity_sso_domain_lookup TO identity_role WITH INHERIT FALSE;
	`
	if _, err := pg.Pool.Exec(ctx, setup); err != nil {
		t.Fatalf("failed to set up identity_role: %v", err)
	}
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("identity_role", "identity_role")+"&search_path=identity,public")
	if err != nil {
		t.Fatalf("failed to connect as identity_role: %v", err)
	}
	defer pool.Close()

	tenant := uuid.New()
	err = pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.CreateSSOConnection(ctx, store.CreateSSOConnectionParams{
			ID: uuid.New(), TenantID: tenant, Protocol: "oidc", Domain: "acme.example", Config: []byte(`{}`),
		}); err != nil {
			return err
		}
		_, err := q.CreateSSOConnection(ctx, store.CreateSSOConnectionParams{
			ID: uuid.New(), TenantID: tenant, Protocol: "oidc", Domain: "off.example", Config: []byte(`{}`),
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE sso_connections SET enabled = false WHERE domain = 'off.example'`)
		return err
	})
	if err != nil {
		t.Fatalf("seeding SSO connections: %v", err)
	}

	lookup := func(domain string) (store.IdentitySsoConnection, error) {
		var got store.IdentitySsoConnection
		err := pool.WithRoleTx(ctx, "identity_sso_domain_lookup", func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = store.New(tx).GetEnabledSSOConnectionByDomain(ctx, domain)
			return err
		})
		return got, err
	}

	t.Run("plain connection with no tenant set sees nothing", func(t *testing.T) {
		_, err := store.New(pool).GetEnabledSSOConnectionByDomain(ctx, "acme.example")
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("got %v, want pgx.ErrNoRows", err)
		}
	})
	t.Run("WithRoleTx finds an enabled domain, case-insensitively", func(t *testing.T) {
		got, err := lookup("ACME.example")
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if got.TenantID != tenant {
			t.Fatalf("got tenant %s, want %s", got.TenantID, tenant)
		}
	})
	t.Run("a disabled connection is not returned", func(t *testing.T) {
		if _, err := lookup("off.example"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("got %v, want pgx.ErrNoRows", err)
		}
	})
	t.Run("the role cannot write", func(t *testing.T) {
		err := pool.WithRoleTx(ctx, "identity_sso_domain_lookup", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE sso_connections SET enabled = false`)
			return err
		})
		if pgErrCode(err) != "42501" {
			t.Fatalf("got %v, want permission denied (42501)", err)
		}
	})
}
