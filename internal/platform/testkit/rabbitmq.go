//go:build integration

package testkit

import (
	"context"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	rabbitmqcontainer "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
)

// StartRabbitMQ starts a RabbitMQ container and returns a connected AMQP
// client. Cleans itself up via t.Cleanup.
//
// No platform/amqp wrapper exists yet — messaging's SMS-engine hand-off
// (system design §5.2) is RabbitMQ's first real consumer, and that's a
// later phase — so this hands back the raw rabbitmq/amqp091-go
// connection the tech stack table (ROADMAP §6) already names.
func StartRabbitMQ(t *testing.T) *amqp.Connection {
	t.Helper()
	ctx := context.Background()

	ctr, err := rabbitmqcontainer.Run(ctx, "rabbitmq:3.12.11-management-alpine",
		rabbitmqcontainer.WithAdminUsername("vaspro"),
		rabbitmqcontainer.WithAdminPassword("vaspro"),
	)
	if err != nil {
		t.Fatalf("testkit: failed to start rabbitmq container: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)

	url, err := ctr.AmqpURL(ctx)
	if err != nil {
		t.Fatalf("testkit: failed to get rabbitmq amqp url: %v", err)
	}

	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("testkit: failed to connect to rabbitmq: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
