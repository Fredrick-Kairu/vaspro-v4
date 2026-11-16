//go:build integration

package bus_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// batchRecorder records every batch a ConsumeBatch handler gets, and
// fails the first failFirst of them.
type batchRecorder struct {
	mu        sync.Mutex
	batches   [][]bus.Message
	failFirst int
}

func (r *batchRecorder) handle(_ context.Context, msgs []bus.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, msgs)
	if len(r.batches) <= r.failFirst {
		return errors.New("insert failed")
	}
	return nil
}

func (r *batchRecorder) snapshot() [][]bus.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]bus.Message(nil), r.batches...)
}

func startBatch(t *testing.T, b *bus.Bus, cfg bus.ConsumeConfig, batch bus.BatchConfig, r *batchRecorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.ConsumeBatch(ctx, cfg, batch, r.handle) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ConsumeBatch returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("ConsumeBatch didn't stop")
		}
	})
}

func publishN(t *testing.T, b *bus.Bus, subject string, n int) {
	t.Helper()
	for i := range n {
		if err := b.Publish(context.Background(), subject, []byte(fmt.Sprintf("m%d", i))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConsumeBatch_DeliversABatchAndAcksIt(t *testing.T) {
	b := testkit.StartNATS(t)
	if err := b.EnsureStream(context.Background(), "BATCH", []string{"batch.>"}); err != nil {
		t.Fatal(err)
	}
	publishN(t, b, "batch.a", 5)
	r := &batchRecorder{}
	startBatch(t, b, bus.ConsumeConfig{Stream: "BATCH", Durable: "batcher", AckWait: time.Second, MaxDeliver: 3},
		bus.BatchConfig{Max: 10, Wait: 300 * time.Millisecond}, r)

	waitFor(t, 5*time.Second, func() bool { return len(r.snapshot()) >= 1 })
	got := r.snapshot()[0]
	if len(got) != 5 || got[0].Subject != "batch.a" || string(got[0].Data) != "m0" || string(got[4].Data) != "m4" {
		t.Fatalf("first batch %+v, want the 5 messages in order with their subject", got)
	}
	time.Sleep(2 * time.Second) // past AckWait: an unacked batch would come back
	for _, batch := range r.snapshot()[1:] {
		if len(batch) != 0 {
			t.Fatalf("acked messages came back: %+v", batch)
		}
	}
}

func TestConsumeBatch_APartialBatchArrivesAfterTheWait(t *testing.T) {
	b := testkit.StartNATS(t)
	if err := b.EnsureStream(context.Background(), "BATCH", []string{"batch.>"}); err != nil {
		t.Fatal(err)
	}
	r := &batchRecorder{}
	startBatch(t, b, bus.ConsumeConfig{Stream: "BATCH", Durable: "batcher", AckWait: time.Second, MaxDeliver: 3},
		bus.BatchConfig{Max: 1000, Wait: 200 * time.Millisecond}, r)
	publishN(t, b, "batch.a", 2)
	waitFor(t, 5*time.Second, func() bool {
		n := 0
		for _, batch := range r.snapshot() {
			n += len(batch)
		}
		return n == 2
	})
}

func TestConsumeBatch_AFailedBatchComesBack(t *testing.T) {
	b := testkit.StartNATS(t)
	if err := b.EnsureStream(context.Background(), "BATCH", []string{"batch.>"}); err != nil {
		t.Fatal(err)
	}
	publishN(t, b, "batch.a", 3)
	r := &batchRecorder{failFirst: 1}
	startBatch(t, b, bus.ConsumeConfig{Stream: "BATCH", Durable: "batcher", AckWait: time.Second, MaxDeliver: 5},
		bus.BatchConfig{Max: 10, Wait: 200 * time.Millisecond}, r)
	waitFor(t, 10*time.Second, func() bool { return len(r.snapshot()) >= 2 })
	batches := r.snapshot()
	if len(batches[0]) != 3 || len(batches[1]) != 3 {
		t.Fatalf("batches %d then %d, want the 3 messages again after the failure", len(batches[0]), len(batches[1]))
	}
}

func TestConsumeBatch_DeadLettersAfterMaxDeliver(t *testing.T) {
	b := testkit.StartNATS(t)
	ctx := context.Background()
	if err := b.EnsureStream(ctx, "BATCH", []string{"batch.>"}); err != nil {
		t.Fatal(err)
	}
	if err := b.EnsureStream(ctx, "BATCH_DLQ", []string{"dlq.batch"}); err != nil {
		t.Fatal(err)
	}
	dead := &batchRecorder{}
	startBatch(t, b, bus.ConsumeConfig{Stream: "BATCH_DLQ", Durable: "dlq", AckWait: time.Second, MaxDeliver: 3},
		bus.BatchConfig{Max: 10, Wait: 200 * time.Millisecond}, dead)
	publishN(t, b, "batch.a", 2)
	r := &batchRecorder{failFirst: 1000}
	startBatch(t, b, bus.ConsumeConfig{Stream: "BATCH", Durable: "batcher", AckWait: time.Second, MaxDeliver: 2, DeadLetterSubject: "dlq.batch"},
		bus.BatchConfig{Max: 10, Wait: 200 * time.Millisecond}, r)
	waitFor(t, 10*time.Second, func() bool {
		n := 0
		for _, batch := range dead.snapshot() {
			n += len(batch)
		}
		return n == 2
	})
	tries := 0
	for _, batch := range r.snapshot() {
		tries += len(batch)
	}
	if tries != 4 {
		t.Fatalf("the failing handler saw %d messages, want each of 2 twice (MaxDeliver)", tries)
	}
}
