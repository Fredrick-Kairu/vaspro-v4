//go:build integration

package testkit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

func TestStartNATS_PublishAndConsumeRoundTrips(t *testing.T) {
	b := testkit.StartNATS(t)
	ctx := context.Background()

	stream := "TESTKIT_STREAM"
	subject := "testkit.events.a"
	if err := b.EnsureStream(ctx, stream, []string{"testkit.events.>"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var received [][]byte
	var mu sync.Mutex
	cc, err := b.Consume(ctx, bus.ConsumeConfig{
		Stream:        stream,
		Durable:       "testkit-consumer",
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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || string(received[0]) != "hello" {
		t.Fatalf("received = %v, want exactly one message %q", received, "hello")
	}
}
