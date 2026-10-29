//go:build integration

package testkit_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

func TestStartPostgres_ConnectsAsSuperuser(t *testing.T) {
	pg := testkit.StartPostgres(t)

	var one int
	if err := pg.Pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if one != 1 {
		t.Fatalf("got %d, want 1", one)
	}
}

func TestStartPostgres_WithMigrations_AppliesThem(t *testing.T) {
	dir := t.TempDir()
	const migration = `-- +goose Up
CREATE TABLE widgets (id int PRIMARY KEY);

-- +goose Down
DROP TABLE widgets;
`
	if err := os.WriteFile(filepath.Join(dir, "00001_widgets.sql"), []byte(migration), 0o600); err != nil {
		t.Fatalf("failed to write migration fixture: %v", err)
	}

	pg := testkit.StartPostgres(t, testkit.WithMigrations(dir))

	var exists bool
	err := pg.Pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'widgets')",
	).Scan(&exists)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatal("expected the widgets table to exist after migrations were applied")
	}
}

func TestPostgres_RoleDSN_ConnectsAsCreatedRole(t *testing.T) {
	pg := testkit.StartPostgres(t)
	ctx := context.Background()

	if _, err := pg.Pool.Exec(ctx, "CREATE ROLE readonly_role LOGIN PASSWORD 'readonly_role'"); err != nil {
		t.Fatalf("failed to create role: %v", err)
	}

	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("readonly_role", "readonly_role"))
	if err != nil {
		t.Fatalf("failed to connect as readonly_role: %v", err)
	}
	defer pool.Close()

	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("unexpected error connecting as the created role: %v", err)
	}
}
