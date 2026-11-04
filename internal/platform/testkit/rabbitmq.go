//go:build integration

package testkit

import (
	"context"
	"fmt"
	"testing"
	"time"

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
	conn := startContainer(t, "rabbitmq", func(ctx context.Context) (*amqp.Connection, testcontainers.Container, error) {
		ctr, err := rabbitmqcontainer.Run(ctx, "rabbitmq:3.12.11-management-alpine",
			rabbitmqcontainer.WithAdminUsername("vaspro"),
			rabbitmqcontainer.WithAdminPassword("vaspro"),
		)
		if err != nil {
			return nil, ctr, fmt.Errorf("starting rabbitmq: %w", err)
		}
		url, err := ctr.AmqpURL(ctx)
		if err != nil {
			return nil, ctr, fmt.Errorf("rabbitmq amqp url: %w", err)
		}
		var conn *amqp.Connection
		err = waitUntilReady(ctx, "rabbitmq", readyTimeout, 200*time.Millisecond, func(context.Context) error {
			conn, err = amqp.Dial(url)
			return err
		})
		return conn, ctr, err
	})
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
