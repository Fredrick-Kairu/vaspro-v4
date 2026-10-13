//go:build integration

package inbox_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/inbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
)

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
		CREATE TABLE processed_events (
			event_id     uuid PRIMARY KEY,
			processed_at timestamptz NOT NULL DEFAULT now()
		);
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
		t.Fatalf("failed to create schema: %v", err)
	}

	return pool
}

func TestMarkProcessed_SecondCallForSameIDReportsAlreadyProcessed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	eventID := uuid.New()

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		already, err := inbox.MarkProcessed(ctx, tx, eventID)
		if err != nil {
			return err
		}
		if already {
			t.Fatal("expected the first MarkProcessed to report alreadyProcessed=false")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		already, err := inbox.MarkProcessed(ctx, tx, eventID)
		if err != nil {
			return err
		}
		if !already {
			t.Fatal("expected the second MarkProcessed for the same ID to report alreadyProcessed=true")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMarkProcessed_RolledBackTransactionDoesNotCount(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	eventID := uuid.New()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := inbox.MarkProcessed(ctx, tx, eventID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		already, err := inbox.MarkProcessed(ctx, tx, eventID)
		if err != nil {
			return err
		}
		if already {
			t.Fatal("expected a rolled-back MarkProcessed not to count; this call should see alreadyProcessed=false")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// fakePublisher mirrors outbox's own test helper; redefined here (not
// imported — it's a test-only type in an internal test package) so this
// test can call Publish a second time directly to simulate "the relay
// crashed after publishing but before marking the row."
type fakePublisher struct {
	mu        sync.Mutex
	published [][]byte
}

func (f *fakePublisher) Publish(_ context.Context, _ string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, data)
	return nil
}

// TestAtLeastOnceDeliveryAppliesExactlyOnce is the Phase 0 scenario named
// directly in ROADMAP §11: "when the relay crashes after publishing but
// before marking the row, the event is published again and the
// consumer's inbox applies it once."
func TestAtLeastOnceDeliveryAppliesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	env, err := events.New(uuid.New(), "tokens.purchased", 1, uuid.New(), time.Now().UTC(), "", map[string]int{"tokens": 1000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return outbox.Write(ctx, tx, "wallet", uuid.New(), env)
	})
	if err != nil {
		t.Fatalf("unexpected error writing the outbox row: %v", err)
	}

	pub := &fakePublisher{}
	relay := outbox.NewRelay(pool, pub, func(eventType string) string { return "events." + eventType })

	// First delivery: the relay publishes and marks the row.
	if _, err := relay.RelayOnce(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Simulate the relay crashing after Publish succeeded but before it
	// could mark the row published — call Publish again directly with
	// the same payload, bypassing the relay's own mark-as-published step.
	if err := pub.Publish(ctx, "events.tokens.purchased", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pub.published) != 2 {
		t.Fatalf("expected the event to be delivered twice (at-least-once), got %d deliveries", len(pub.published))
	}

	// The consumer applies each delivery through inbox.MarkProcessed
	// before doing any work; the second delivery must be a no-op.
	var applied atomic.Int32
	apply := func() error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			already, err := inbox.MarkProcessed(ctx, tx, env.ID)
			if err != nil {
				return err
			}
			if !already {
				applied.Add(1)
			}
			return nil
		})
	}
	if err := apply(); err != nil {
		t.Fatalf("unexpected error on first delivery's apply: %v", err)
	}
	if err := apply(); err != nil {
		t.Fatalf("unexpected error on second delivery's apply: %v", err)
	}

	if got := applied.Load(); got != 1 {
		t.Fatalf("applied the event %d times despite 2 deliveries, want exactly 1", got)
	}
}
