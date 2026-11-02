//go:build integration

package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver, used only to run goose migrations
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
)

const postgresTestDatabase = "vaspro_test"

// readyTimeout is how long a Start* helper waits for its container to
// accept a real client connection after the container's own wait
// strategy has passed (see waitUntilReady).
const readyTimeout = 30 * time.Second

// Postgres is a running PostgreSQL test container, connected as its
// superuser.
type Postgres struct {
	Pool *vasdb.Pool

	dsn  string
	host string
	port string
}

type postgresConfig struct {
	migrationsDir string
}

// PostgresOption configures StartPostgres.
type PostgresOption func(*postgresConfig)

// WithMigrations applies every migration in dir (via goose) before
// StartPostgres returns. Omit it for a bare container — the caller sets
// up its own schema, same as every integration test did before this
// package existed.
func WithMigrations(dir string) PostgresOption {
	return func(c *postgresConfig) { c.migrationsDir = dir }
}

// StartPostgres starts a postgres:16-alpine container and returns a
// Postgres connected as its superuser. Cleans itself up via t.Cleanup.
func StartPostgres(t *testing.T, opts ...PostgresOption) *Postgres {
	t.Helper()
	var cfg postgresConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	ctx := context.Background()

	// The official postgres image logs "database system is ready to
	// accept connections" once for a temporary setup server and again
	// for the real one; waiting for only the first occurrence races the
	// restart in between and produces "connection reset by peer".
	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase(postgresTestDatabase),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("testkit: failed to start postgres container: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("testkit: failed to get postgres connection string: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("testkit: failed to get postgres host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("testkit: failed to get postgres port: %v", err)
	}

	if err := waitUntilReady(ctx, "postgres", readyTimeout, 200*time.Millisecond, func(ctx context.Context) error {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.Background()) }()
		return conn.Ping(ctx)
	}); err != nil {
		t.Fatalf("testkit: %v", err)
	}

	if cfg.migrationsDir != "" {
		if err := applyMigrations(ctx, dsn, cfg.migrationsDir); err != nil {
			t.Fatalf("testkit: applying migrations from %s: %v", cfg.migrationsDir, err)
		}
	}

	pool, err := vasdb.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("testkit: failed to connect as superuser: %v", err)
	}
	t.Cleanup(pool.Close)

	return &Postgres{Pool: pool, dsn: dsn, host: host, port: port.Port()}
}

// DSN returns the superuser connection string.
func (p *Postgres) DSN() string { return p.dsn }

// RoleDSN builds a connection string for a different role against the
// same container and database — e.g. a low-privilege role an RLS test
// creates via p.Pool.Exec("CREATE ROLE ...") and then connects as, to
// stand in for what a real service would connect as.
func (p *Postgres) RoleDSN(user, password string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, password, p.host, p.port, postgresTestDatabase)
}

// applyMigrations runs every migration in dir against dsn using goose's
// per-instance Provider API (not its deprecated global functions, which
// share package-level state — two tests starting Postgres concurrently
// must not race on that).
func applyMigrations(ctx context.Context, dsn, dir string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("opening migration connection: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(dir))
	if err != nil {
		return fmt.Errorf("building goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}
