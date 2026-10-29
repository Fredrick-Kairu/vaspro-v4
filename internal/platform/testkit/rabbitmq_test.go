//go:build integration

package testkit_test

import (
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

func TestStartRabbitMQ_PublishAndConsumeRoundTrips(t *testing.T) {
	conn := testkit.StartRabbitMQ(t)

	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("failed to open channel: %v", err)
	}
	defer func() { _ = ch.Close() }()

	q, err := ch.QueueDeclare("testkit-queue", false, true, false, false, nil)
	if err != nil {
		t.Fatalf("failed to declare queue: %v", err)
	}

	msgs, err := ch.Consume(q.Name, "", true, false, false, false, nil)
	if err != nil {
		t.Fatalf("failed to start consuming: %v", err)
	}

	if err := ch.Publish("", q.Name, false, false, amqp.Publishing{Body: []byte("hello")}); err != nil {
		t.Fatalf("failed to publish: %v", err)
	}

	select {
	case msg := <-msgs:
		if string(msg.Body) != "hello" {
			t.Fatalf("got %q, want %q", msg.Body, "hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the published message")
	}
}
