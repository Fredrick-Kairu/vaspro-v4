//go:build integration

package bus_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// waitFor polls cond until it's true or timeout elapses, instead of a
// fixed sleep (.claude/rules/tests.md: "no sleeps to wait for async work;
// poll with a timeout helper").
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func TestPublishAndConsume_RoundTrips(t *testing.T) {
	ctx := context.Background()
	b := testkit.StartNATS(t)

	stream := "TEST_STREAM"
	subject := "test.events.a"
	if err := b.EnsureStream(ctx, stream, []string{"test.events.>"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var received [][]byte
	var mu sync.Mutex
	cc, err := b.Consume(ctx, bus.ConsumeConfig{
		Stream:        stream,
		Durable:       "test-consumer",
		FilterSubject: subject,
		AckWait:       2 * time.Second,
		MaxDeliver:    3,
	}, func(_ context.Context, data []byte) error {
		mu.Lock()
		received = append(received, data)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cc.Stop()

	if err := b.Publish(ctx, subject, []byte("hello")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	})

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || string(received[0]) != "hello" {
		t.Fatalf("received = %v, want exactly one message %q", received, "hello")
	}
}

func TestConsume_RetriesOnFailureThenSucceeds(t *testing.T) {
	ctx := context.Background()
	b := testkit.StartNATS(t)

	stream := "TEST_STREAM"
	subject := "test.events.retry"
	if err := b.EnsureStream(ctx, stream, []string{"test.events.>"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var calls atomic.Int32
	cc, err := b.Consume(ctx, bus.ConsumeConfig{
		Stream:        stream,
		Durable:       "retry-consumer",
		FilterSubject: subject,
		AckWait:       2 * time.Second,
		MaxDeliver:    5,
	}, func(_ context.Context, _ []byte) error {
		n := calls.Add(1)
		if n < 3 {
			return context.DeadlineExceeded // any error triggers a retry
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cc.Stop()

	if err := b.Publish(ctx, subject, []byte("retry-me")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitFor(t, 10*time.Second, func() bool { return calls.Load() == 3 })

	// Give a little more time to confirm it stops at exactly 3 (a 4th
	// call would mean Ack didn't actually take effect).
	time.Sleep(500 * time.Millisecond)
	if got := calls.Load(); got != 3 {
		t.Fatalf("handle was called %d times, want exactly 3", got)
	}
}

func TestConsume_DeadLettersAfterMaxDeliver(t *testing.T) {
	ctx := context.Background()
	b := testkit.StartNATS(t)

	stream := "TEST_STREAM"
	subject := "test.events.dlq-source"
	dlq := "test.events.dlq"
	if err := b.EnsureStream(ctx, stream, []string{"test.events.>"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var calls atomic.Int32
	cc, err := b.Consume(ctx, bus.ConsumeConfig{
		Stream:            stream,
		Durable:           "dlq-consumer",
		FilterSubject:     subject,
		AckWait:           time.Second,
		MaxDeliver:        3,
		DeadLetterSubject: dlq,
	}, func(_ context.Context, _ []byte) error {
		calls.Add(1)
		return context.DeadlineExceeded // always fails
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cc.Stop()

	var dlqReceived [][]byte
	var mu sync.Mutex
	dlqCC, err := b.Consume(ctx, bus.ConsumeConfig{
		Stream:        stream,
		Durable:       "dlq-watcher",
		FilterSubject: dlq,
		AckWait:       2 * time.Second,
		MaxDeliver:    1,
	}, func(_ context.Context, data []byte) error {
		mu.Lock()
		dlqReceived = append(dlqReceived, data)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer dlqCC.Stop()

	if err := b.Publish(ctx, subject, []byte("doomed")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitFor(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(dlqReceived) == 1
	})

	if got := calls.Load(); got != 3 {
		t.Fatalf("handle was called %d times, want exactly 3 (MaxDeliver)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dlqReceived) != 1 || string(dlqReceived[0]) != "doomed" {
		t.Fatalf("dead-lettered message = %v, want exactly one %q", dlqReceived, "doomed")
	}
}

func TestBusPublish_SatisfiesOutboxPublisherAndRelays(t *testing.T) {
	ctx := context.Background()
	b := testkit.StartNATS(t)

	var _ outbox.Publisher = b // compile-time proof, checked again at runtime below

	stream := "TEST_STREAM"
	subject := "test.events.outbox-relayed"
	if err := b.EnsureStream(ctx, stream, []string{"test.events.>"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pool := newOutboxPool(t)

	env, err := events.New(uuid.New(), "outbox-relayed", 1, uuid.New(), time.Now().UTC(), "", map[string]int{"n": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := writeViaOutbox(ctx, t, pool, env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var received [][]byte
	var mu sync.Mutex
	cc, err := b.Consume(ctx, bus.ConsumeConfig{
		Stream:        stream,
		Durable:       "outbox-watcher",
		FilterSubject: subject,
		AckWait:       2 * time.Second,
		MaxDeliver:    3,
	}, func(_ context.Context, data []byte) error {
		mu.Lock()
		received = append(received, data)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cc.Stop()

	relay := outbox.NewRelay(pool, b, func(eventType string) string { return "test.events." + eventType })
	if _, err := relay.RelayOnce(ctx); err != nil {
		t.Fatalf("unexpected error relaying: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	})

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("received %d messages, want exactly 1", len(received))
	}
}

func writeViaOutbox(ctx context.Context, t *testing.T, pool *vasdb.Pool, env events.Envelope) error {
	t.Helper()
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return outbox.Write(ctx, tx, "test", uuid.New(), env)
	})
}

func newOutboxPool(t *testing.T) *vasdb.Pool {
	t.Helper()
	ctx := context.Background()

	pool := testkit.StartPostgres(t).Pool

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
