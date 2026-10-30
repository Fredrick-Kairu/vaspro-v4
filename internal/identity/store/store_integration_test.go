//go:build integration

package store_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
		ID:         uuid.New(),
		UserID:     user.ID,
		ExpiresAt:  time.Now().Add(time.Hour),
		AuthMethod: "password",
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

// TestSessions_TokenHashUnique covers migration 20261025090000's
// sessions_token_hash index: two sessions can never share a token hash,
// while any number of rows with no token (created before the column
// existed) can coexist.
func TestSessions_TokenHashUnique(t *testing.T) {
	ctx := context.Background()
	pool := newIdentityPool(t)
	q := store.New(pool)

	user, err := q.CreateUser(ctx, store.CreateUserParams{
		ID:       uuid.New(),
		Email:    "token-owner@example.com",
		FullName: "Token Owner",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	create := func(tokenHash []byte) error {
		_, err := q.CreateSession(ctx, store.CreateSessionParams{
			ID:         uuid.New(),
			UserID:     user.ID,
			ExpiresAt:  time.Now().Add(time.Hour),
			TokenHash:  tokenHash,
			AuthMethod: "password",
		})
		return err
	}

	for i := 0; i < 2; i++ {
		if err := create(nil); err != nil {
			t.Fatalf("session %d with no token hash: %v", i, err)
		}
	}

	hash := []byte("0123456789abcdef0123456789abcdef")
	if err := create(hash); err != nil {
		t.Fatalf("first session with a token hash: %v", err)
	}
	var pgErr *pgconn.PgError
	if err := create(hash); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("got error %v, want a unique violation (23505) for a duplicate token hash", err)
	}
}

// TestClaimTOTPStep_OnlyMovesForward covers migration 20261026090000's
// user_totp.last_used_step: a step is claimed at most once, and never
// one at or before the last claimed (RFC 6238 §5.2). Re-enrolling
// clears it, since a new secret has no used codes.
func TestClaimTOTPStep_OnlyMovesForward(t *testing.T) {
	ctx := context.Background()
	pool := newIdentityPool(t)
	q := store.New(pool)

	user, err := q.CreateUser(ctx, store.CreateUserParams{
		ID:       uuid.New(),
		Email:    "totp-owner@example.com",
		FullName: "TOTP Owner",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := q.CreateUserTOTP(ctx, store.CreateUserTOTPParams{UserID: user.ID, SecretCiphertext: []byte("sealed")}); err != nil {
		t.Fatalf("CreateUserTOTP: %v", err)
	}
	claim := func(step int64) int64 {
		t.Helper()
		n, err := q.ClaimTOTPStep(ctx, store.ClaimTOTPStepParams{UserID: user.ID, Step: step})
		if err != nil {
			t.Fatalf("ClaimTOTPStep(%d): %v", step, err)
		}
		return n
	}

	steps := []struct {
		name string
		step int64
		want int64
	}{
		{"first use", 100, 1},
		{"same step again", 100, 0},
		{"an earlier step", 99, 0},
		{"a later step", 101, 1},
	}
	for _, s := range steps {
		if got := claim(s.step); got != s.want {
			t.Fatalf("%s (step %d): claimed %d rows, want %d", s.name, s.step, got, s.want)
		}
	}

	if _, err := q.UpsertUserTOTP(ctx, store.UpsertUserTOTPParams{UserID: user.ID, SecretCiphertext: []byte("resealed"), RecoveryCodeHashes: []string{}}); err != nil {
		t.Fatalf("UpsertUserTOTP: %v", err)
	}
	if got := claim(50); got != 1 {
		t.Fatalf("after re-enrolling: claimed %d rows for step 50, want 1", got)
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

func TestSigningKeys_RotationQueries(t *testing.T) {
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

	active, err := q.GetActiveSigningKey(ctx)
	if err != nil {
		t.Fatalf("GetActiveSigningKey: %v", err)
	}
	if active.Kid != "kid-1" {
		t.Fatalf("got active kid %q, want kid-1", active.Kid)
	}

	demotedAt := time.Now().UTC()
	if err := q.DemoteSigningKeyToRetiring(ctx, store.DemoteSigningKeyToRetiringParams{
		Kid: "kid-1",
		Now: demotedAt,
	}); err != nil {
		t.Fatalf("DemoteSigningKeyToRetiring: %v", err)
	}

	if _, err := q.CreateSigningKey(ctx, store.CreateSigningKeyParams{
		Kid:        "kid-2",
		Algorithm:  "EdDSA",
		PublicKey:  []byte("pub-2"),
		PrivateRef: "kms://key-2",
		Status:     "active",
	}); err != nil {
		t.Fatalf("CreateSigningKey(kid-2): %v", err)
	}

	publishable, err := q.ListPublishableSigningKeys(ctx)
	if err != nil {
		t.Fatalf("ListPublishableSigningKeys: %v", err)
	}
	var publishableKids []string
	for _, k := range publishable {
		publishableKids = append(publishableKids, k.Kid)
	}
	if len(publishableKids) != 2 {
		t.Fatalf("got publishable kids %v, want [kid-1 kid-2] (both active+retiring)", publishableKids)
	}

	// Not yet past the overlap window relative to demotedAt: nothing to retire.
	affected, err := q.RetireStaleSigningKeys(ctx, demotedAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("RetireStaleSigningKeys (before cutoff): %v", err)
	}
	if affected != 0 {
		t.Fatalf("got %d retired before the cutoff, want 0", affected)
	}

	// Past the overlap window: kid-1 (retiring since demotedAt) is retired.
	affected, err = q.RetireStaleSigningKeys(ctx, demotedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("RetireStaleSigningKeys (after cutoff): %v", err)
	}
	if affected != 1 {
		t.Fatalf("got %d retired after the cutoff, want 1", affected)
	}

	publishable, err = q.ListPublishableSigningKeys(ctx)
	if err != nil {
		t.Fatalf("ListPublishableSigningKeys after retiring: %v", err)
	}
	if len(publishable) != 1 || publishable[0].Kid != "kid-2" {
		t.Fatalf("got %v, want exactly [kid-2]", publishable)
	}
}

// TestTokenExchangeRole_CanReadApiClientsAcrossTenants proves migration
// 20261022223744's grants/policy actually work for a realistic,
// non-superuser app connection — not just that a superuser test
// connection can see everything regardless. identity_role mirrors
// TestAssertRowLevelSecurity_OnAPIClients's own setup (blanket CRUD on
// the schema, same as a real app connection), plus the one extra
// GRANT a real deployment's migration-running role would already have
// picked up automatically from "GRANT identity_token_exchange TO
// CURRENT_USER" (migration 20261022223744) — granted explicitly here
// since this test's migration ran as the superuser, not identity_role.
func TestTokenExchangeRole_CanReadApiClientsAcrossTenants(t *testing.T) {
	ctx := context.Background()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/identity"))

	const setup = `
		CREATE ROLE identity_role LOGIN PASSWORD 'identity_role';
		GRANT USAGE ON SCHEMA identity TO identity_role;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO identity_role;
		GRANT identity_token_exchange TO identity_role WITH INHERIT FALSE;
	`
	if _, err := pg.Pool.Exec(ctx, setup); err != nil {
		t.Fatalf("failed to set up identity_role: %v", err)
	}

	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("identity_role", "identity_role")+"&search_path=identity,public")
	if err != nil {
		t.Fatalf("failed to connect as identity_role: %v", err)
	}
	defer pool.Close()

	tenantA := uuid.New()
	clientID := uuid.New()
	err = pool.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).CreateApiClient(ctx, store.CreateApiClientParams{
			ID:          clientID,
			TenantID:    tenantA,
			Name:        "token exchange test client",
			Environment: "sandbox",
			Scopes:      []string{"sms:send"},
			IpAllowlist: []netip.Prefix{},
			CreatedBy:   uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seeding an api client: %v", err)
	}

	// WITH INHERIT FALSE above is what this proves: a default
	// (inheriting) GRANT would make identity_token_exchange's SELECT
	// policy match on every query identity_role ever runs, not just
	// inside WithRoleTx — the exact bug this test caught once, before
	// the migration's GRANT had WITH INHERIT FALSE added.
	t.Run("plain connection with no tenant set sees nothing", func(t *testing.T) {
		_, err := store.New(pool).GetApiClientByIDAnyTenant(ctx, clientID)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("got err %v, want pgx.ErrNoRows (FORCE ROW LEVEL SECURITY, no app.tenant_id set)", err)
		}
	})

	t.Run("WithRoleTx as identity_token_exchange can see it", func(t *testing.T) {
		var got store.IdentityApiClient
		err := pool.WithRoleTx(ctx, "identity_token_exchange", func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = store.New(tx).GetApiClientByIDAnyTenant(ctx, clientID)
			return err
		})
		if err != nil {
			t.Fatalf("GetApiClientByIDAnyTenant inside WithRoleTx: %v", err)
		}
		if got.ID != clientID || got.TenantID != tenantA {
			t.Fatalf("got client %+v, want id=%s tenant=%s", got, clientID, tenantA)
		}
	})
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
