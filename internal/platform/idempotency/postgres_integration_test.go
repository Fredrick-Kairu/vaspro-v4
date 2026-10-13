//go:build integration

package idempotency_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
)

// assertJSONEqual compares got and want by value, not by byte, since
// the response column is jsonb (data model §1) and Postgres re-serializes
// JSON on storage — e.g. {"id":"abc"} comes back as {"id": "abc"}, which
// is a different byte string but the same JSON.
func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("got is not valid JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("want is not valid JSON: %v (%s)", err, want)
	}
	gotNorm, _ := json.Marshal(gotVal)
	wantNorm, _ := json.Marshal(wantVal)
	if string(gotNorm) != string(wantNorm) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func newTestStore(t *testing.T) idempotency.Store {
	t.Helper()
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("vaspro_test"),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}

	pool, err := vasdb.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const schema = `
		CREATE TABLE idempotency_keys (
			tenant_id    uuid NOT NULL,
			key          text NOT NULL,
			request_hash bytea NOT NULL,
			status_code  int,
			response     jsonb,
			created_at   timestamptz NOT NULL DEFAULT now(),
			expires_at   timestamptz NOT NULL,
			PRIMARY KEY (tenant_id, key)
		);
	`
	if _, err := pool.Exec(ctx, schema); err != nil {
		t.Fatalf("failed to create idempotency_keys: %v", err)
	}

	return idempotency.NewPostgresStore(pool)
}

func TestPostgresStore_ClaimThenComplete_Replays(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	tenantID := uuid.New()
	hash := []byte("hash-of-the-request-body")

	existing, err := store.Claim(ctx, tenantID, "key-1", hash, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if existing != nil {
		t.Fatalf("expected a fresh claim to return nil, got %+v", existing)
	}

	if err := store.Complete(ctx, tenantID, "key-1", 201, []byte(`{"id":"abc"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	replay, err := store.Claim(ctx, tenantID, "key-1", hash, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if replay == nil {
		t.Fatal("expected the second claim to return the existing record")
	}
	if !replay.Complete {
		t.Fatal("expected the existing record to be Complete")
	}
	if replay.StatusCode != 201 {
		t.Fatalf("StatusCode = %d, want 201", replay.StatusCode)
	}
	assertJSONEqual(t, replay.Response, []byte(`{"id":"abc"}`))
}

func TestPostgresStore_DifferentTenants_DontCollide(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	hash := []byte("same-hash")

	tenantA, tenantB := uuid.New(), uuid.New()

	if _, err := store.Claim(ctx, tenantA, "key-1", hash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Complete(ctx, tenantA, "key-1", 200, []byte(`{}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	existing, err := store.Claim(ctx, tenantB, "key-1", hash, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if existing != nil {
		t.Fatal("expected tenant B's claim on the same key string to be independent of tenant A's")
	}
}

func TestPostgresStore_ExpiredKey_CanBeReclaimed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	tenantID := uuid.New()

	// Claim and complete with an expiry already in the past.
	if _, err := store.Claim(ctx, tenantID, "key-1", []byte("old-hash"), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Complete(ctx, tenantID, "key-1", 200, []byte(`{"old":true}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// A fresh claim for the same key, now expired, must succeed as if
	// new — "keys expire as configured" (ROADMAP's Phase 0 test list).
	existing, err := store.Claim(ctx, tenantID, "key-1", []byte("new-hash"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if existing != nil {
		t.Fatalf("expected an expired key to be reclaimable as a fresh claim, got %+v", existing)
	}

	// And it should behave as genuinely fresh: completing it with a new
	// response, then claiming again, replays the *new* response, not
	// the expired one.
	if err := store.Complete(ctx, tenantID, "key-1", 201, []byte(`{"new":true}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	replay, err := store.Claim(ctx, tenantID, "key-1", []byte("new-hash"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if replay == nil {
		t.Fatal("expected a replay record after re-completing the reclaimed key")
	}
	assertJSONEqual(t, replay.Response, []byte(`{"new":true}`))
}

func TestPostgresStore_StillLiveKey_IsNotReclaimed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	tenantID := uuid.New()

	if _, err := store.Claim(ctx, tenantID, "key-1", []byte("hash-a"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Complete(ctx, tenantID, "key-1", 200, []byte(`{"a":true}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	existing, err := store.Claim(ctx, tenantID, "key-1", []byte("hash-b"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if existing == nil {
		t.Fatal("expected a still-live key not to be reclaimed")
	}
	if string(existing.RequestHash) != "hash-a" {
		t.Fatalf("expected the original hash to survive, got %q", existing.RequestHash)
	}
}
