//go:build integration

package testkit_test

import (
	"database/sql"
	"testing"

	_ "github.com/ClickHouse/clickhouse-go/v2" // registers the "clickhouse" database/sql driver

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

func TestStartClickHouse_AnswersAQuery(t *testing.T) {
	dsn := testkit.StartClickHouse(t)
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var one int
	if err := db.QueryRowContext(t.Context(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 = %d, %v", one, err)
	}
}

func TestStartClickHouse_WithMigrations(t *testing.T) {
	dsn := testkit.StartClickHouse(t, testkit.WithClickHouseMigrations("../../../migrations/clickhouse"))
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var engine string
	if err := db.QueryRowContext(t.Context(), "SELECT engine FROM system.tables WHERE database = currentDatabase() AND name = 'events'").Scan(&engine); err != nil {
		t.Fatalf("the events table: %v", err)
	}
	if engine != "ReplacingMergeTree" {
		t.Fatalf("engine %q", engine)
	}
}
