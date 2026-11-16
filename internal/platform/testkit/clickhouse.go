//go:build integration

package testkit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // registers the "clickhouse" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	clickhousecontainer "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/testcontainers/testcontainers-go/wait"
)

// clickHouseImage matches deploy/compose's.
const clickHouseImage = "clickhouse/clickhouse-server:24.3-alpine"

type clickHouseConfig struct {
	migrationsDir string
}

// ClickHouseOption configures StartClickHouse.
type ClickHouseOption func(*clickHouseConfig)

// WithClickHouseMigrations applies every migration in dir (goose, the
// clickhouse dialect) before StartClickHouse returns.
func WithClickHouseMigrations(dir string) ClickHouseOption {
	return func(c *clickHouseConfig) { c.migrationsDir = dir }
}

// StartClickHouse starts a ClickHouse container and returns a DSN for
// its native protocol (clickhouse://user:pass@host:port/db), ready for
// clickhouse-go's database/sql driver. Cleans itself up via t.Cleanup.
func StartClickHouse(t *testing.T, opts ...ClickHouseOption) string {
	t.Helper()
	var cfg clickHouseConfig
	for _, o := range opts {
		o(&cfg)
	}
	dsn := startContainer(t, "clickhouse", func(ctx context.Context) (string, testcontainers.Container, error) {
		ctr, err := clickhousecontainer.Run(ctx, clickHouseImage,
			clickhousecontainer.WithUsername("vaspro"),
			clickhousecontainer.WithPassword("vaspro"),
			clickhousecontainer.WithDatabase("vaspro"),
			// The module's HTTP wait has no deadline of its own; give it
			// the 60s the other containers get under a full parallel run.
			testcontainers.WithWaitStrategy(wait.NewHTTPStrategy("/ping").WithPort("8123/tcp").WithStartupTimeout(60*time.Second)),
		)
		if err != nil {
			return "", ctr, fmt.Errorf("starting clickhouse: %w", err)
		}
		dsn, err := ctr.ConnectionString(ctx)
		if err != nil {
			return "", ctr, fmt.Errorf("clickhouse connection string: %w", err)
		}
		return dsn, ctr, waitUntilReady(ctx, "clickhouse", readyTimeout, 200*time.Millisecond, func(ctx context.Context) error {
			db, err := sql.Open("clickhouse", dsn)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			return db.PingContext(ctx)
		})
	})
	if cfg.migrationsDir != "" {
		if err := applyClickHouseMigrations(context.Background(), dsn, cfg.migrationsDir); err != nil {
			t.Fatalf("testkit: applying clickhouse migrations from %s: %v", cfg.migrationsDir, err)
		}
	}
	return dsn
}

func applyClickHouseMigrations(ctx context.Context, dsn, dir string) error {
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return fmt.Errorf("opening migration connection: %w", err)
	}
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectClickHouse, db, os.DirFS(dir))
	if err != nil {
		return fmt.Errorf("building goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}
