//go:build integration

package outbox_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
)

// newTestPool starts a postgres container and creates outbox_events
// exactly per data model §1 — this table isn't RLS-protected (the relay
// reads across every tenant by design, which is why tenant_id is
// nullable), so a plain superuser pool is fine here, unlike platform/db's
// RLS test which needs a dedicated low-privilege role.
func newTestPool(t *testing.T) *vasdb.Pool {
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
		CREATE TABLE outbox_events (
			id           uuid PRIMARY KEY,
			tenant_id    uuid,
			aggregate    text NOT NULL,
			aggregate_id uuid NOT NULL,
			event_type   text NOT NULL,
			payload      jsonb NOT NULL,
			occurred_at  timestamptz NOT NULL DEFAULT now(),
			published_at timestamptz
		);
		CREATE INDEX outbox_unpublished ON outbox_events (occurred_at) WHERE published_at IS NULL;
	`
	if _, err := pool.Exec(ctx, schema); err != nil {
		t.Fatalf("failed to create outbox_events: %v", err)
	}

	return pool
}

type fakePublisher struct {
	mu        sync.Mutex
	published []string // subjects, in call order
	failFor   map[string]bool
}

func (f *fakePublisher) Publish(_ context.Context, subject string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFor[subject] {
		return context.DeadlineExceeded // any error; the content doesn't matter to these tests
	}
	f.published = append(f.published, subject)
	return nil
}

func (f *fakePublisher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

func testEnvelope(t *testing.T, eventType string, occurredAt time.Time) events.Envelope {
	t.Helper()
	env, err := events.New(uuid.New(), eventType, 1, uuid.New(), occurredAt, "", map[string]int{"n": 1})
	if err != nil {
		t.Fatalf("unexpected error building envelope: %v", err)
	}
	return env
}

func countRows(t *testing.T, pool *vasdb.Pool, ctx context.Context) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events").Scan(&n); err != nil {
		t.Fatalf("unexpected error counting rows: %v", err)
	}
	return n
}

func TestWrite_OnlyPersistsIfTheTransactionCommits(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env := testEnvelope(t, "test.rolled_back", time.Now().UTC())
	if err := outbox.Write(ctx, tx, "test", uuid.New(), env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if n := countRows(t, pool, ctx); n != 0 {
		t.Fatalf("expected 0 rows after rollback, got %d", n)
	}

	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env2 := testEnvelope(t, "test.committed", time.Now().UTC())
	if err := outbox.Write(ctx, tx2, "test", uuid.New(), env2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if n := countRows(t, pool, ctx); n != 1 {
		t.Fatalf("expected 1 row after commit, got %d", n)
	}

	var publishedAt *time.Time
	err = pool.QueryRow(ctx, "SELECT published_at FROM outbox_events WHERE id = $1", env2.ID).Scan(&publishedAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if publishedAt != nil {
		t.Fatal("expected a freshly written row to have published_at NULL")
	}
}

func writeCommitted(t *testing.T, pool *vasdb.Pool, ctx context.Context, env events.Envelope) {
	t.Helper()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return outbox.Write(ctx, tx, "test", uuid.New(), env)
	})
	if err != nil {
		t.Fatalf("unexpected error writing %v: %v", env.ID, err)
	}
}

func TestRelayOnce_PublishesOldestFirstAndMarksPublished(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	base := time.Now().UTC()
	envA := testEnvelope(t, "a", base)
	envB := testEnvelope(t, "b", base.Add(time.Second))
	writeCommitted(t, pool, ctx, envB) // write B first, A second...
	writeCommitted(t, pool, ctx, envA) // ...but A occurred earlier and must relay first

	pub := &fakePublisher{}
	relay := outbox.NewRelay(pool, pub, func(eventType string) string { return "events." + eventType })

	n, err := relay.RelayOnce(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("relayed %d rows, want 2", n)
	}
	if got := pub.published; len(got) != 2 || got[0] != "events.a" || got[1] != "events.b" {
		t.Fatalf("published order = %v, want [events.a events.b]", got)
	}

	// Already published; a second pass must not relay either again.
	n2, err := relay.RelayOnce(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second RelayOnce relayed %d rows, want 0", n2)
	}
	if pub.callCount() != 2 {
		t.Fatalf("Publisher was called %d times, want 2", pub.callCount())
	}
}

func TestRelayOnce_LeavesAFailedPublishUnmarked(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	envOK := testEnvelope(t, "ok", time.Now().UTC())
	envFail := testEnvelope(t, "fail", time.Now().UTC().Add(time.Second))
	writeCommitted(t, pool, ctx, envOK)
	writeCommitted(t, pool, ctx, envFail)

	pub := &fakePublisher{failFor: map[string]bool{"events.fail": true}}
	relay := outbox.NewRelay(pool, pub, func(eventType string) string { return "events." + eventType })

	n, err := relay.RelayOnce(ctx)
	if err == nil {
		t.Fatal("expected RelayOnce to report the publish failure")
	}
	if n != 1 {
		t.Fatalf("relayed %d rows, want 1 (the one that succeeded)", n)
	}

	var publishedAt *time.Time
	if err := pool.QueryRow(ctx, "SELECT published_at FROM outbox_events WHERE id = $1", envFail.ID).Scan(&publishedAt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if publishedAt != nil {
		t.Fatal("expected the failed row to remain unpublished so a later pass retries it")
	}
}
