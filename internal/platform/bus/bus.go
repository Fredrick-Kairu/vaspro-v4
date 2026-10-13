// Package bus wraps NATS JetStream: durable publish, durable consumers
// with retries, and a hand-rolled dead-letter subject (OD-01; JetStream
// has no built-in "move failed messages to this subject" config at the
// client API level, so Consume implements it directly).
package bus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Bus is a connected NATS JetStream client.
type Bus struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// Connect dials url and wraps it with JetStream. nats.go's Connect has
// no context-based variant; ctx is accepted anyway for consistency with
// every other platform/* constructor (db.NewPool, cache.New's callers).
func Connect(_ context.Context, url string) (*Bus, error) {
	nc, err := nats.Connect(url)
	if err != nil {
		return nil, fmt.Errorf("bus: connecting: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bus: wrapping jetstream: %w", err)
	}
	return &Bus{nc: nc, js: js}, nil
}

// Close closes the underlying connection.
func (b *Bus) Close() { b.nc.Close() }

// Publish sends data on subject through JetStream. Satisfies
// platform/outbox.Publisher directly.
func (b *Bus) Publish(ctx context.Context, subject string, data []byte) error {
	if _, err := b.js.Publish(ctx, subject, data); err != nil {
		return fmt.Errorf("bus: publishing to %s: %w", subject, err)
	}
	return nil
}

// EnsureStream creates or updates a stream named name covering subjects.
// Idempotent — safe to call on every service start-up.
func (b *Bus) EnsureStream(ctx context.Context, name string, subjects []string) error {
	_, err := b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     name,
		Subjects: subjects,
	})
	if err != nil {
		return fmt.Errorf("bus: ensuring stream %s: %w", name, err)
	}
	return nil
}

// ConsumeConfig configures a durable pull consumer.
type ConsumeConfig struct {
	Stream        string
	Durable       string
	FilterSubject string
	AckWait       time.Duration
	// MaxDeliver is how many times a message is attempted (including the
	// first) before it's dead-lettered (or, with DeadLetterSubject
	// empty, simply stops being redelivered by JetStream itself).
	MaxDeliver int
	// DeadLetterSubject, if set, receives the raw message body as-is on
	// the final failed attempt, published before the original is Term'd.
	DeadLetterSubject string
}

// Consume creates or binds a durable consumer per cfg and runs handle for
// every message it delivers. handle returning nil Acks the message;
// returning an error Naks it for redelivery, unless this was the final
// attempt allowed by cfg.MaxDeliver, in which case the message is
// published to cfg.DeadLetterSubject (if set) and Term'd instead of
// Nak'd — ending redelivery regardless of what MaxDeliver would
// otherwise still allow.
func (b *Bus) Consume(ctx context.Context, cfg ConsumeConfig, handle func(ctx context.Context, data []byte) error) (jetstream.ConsumeContext, error) {
	consumer, err := b.js.CreateOrUpdateConsumer(ctx, cfg.Stream, jetstream.ConsumerConfig{
		Durable:       cfg.Durable,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxDeliver:    cfg.MaxDeliver,
		FilterSubject: cfg.FilterSubject,
	})
	if err != nil {
		return nil, fmt.Errorf("bus: creating consumer %s on %s: %w", cfg.Durable, cfg.Stream, err)
	}

	cc, err := consumer.Consume(func(msg jetstream.Msg) {
		if err := handle(ctx, msg.Data()); err == nil {
			_ = msg.Ack()
			return
		}

		if cfg.MaxDeliver > 0 {
			if meta, metaErr := msg.Metadata(); metaErr == nil && meta.NumDelivered >= uint64(cfg.MaxDeliver) {
				if cfg.DeadLetterSubject != "" {
					_ = b.Publish(ctx, cfg.DeadLetterSubject, msg.Data())
				}
				_ = msg.Term()
				return
			}
		}
		_ = msg.Nak()
	})
	if err != nil {
		return nil, fmt.Errorf("bus: starting consume on %s: %w", cfg.Durable, err)
	}
	return cc, nil
}
