//go:build integration

package testkit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/testcontainers/testcontainers-go"
	natscontainer "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
)

// StartNATS starts a NATS JetStream container and returns a connected
// Bus. Cleans itself up via t.Cleanup.
func StartNATS(t *testing.T) *bus.Bus {
	t.Helper()
	url := StartNATSURL(t)

	b, err := bus.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("testkit: failed to connect to nats: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

// StartNATSURL starts a NATS JetStream container and returns its
// connection URL, for a caller that needs to hand it to a separate
// process (e.g. a built service binary under test) rather than an
// in-process *bus.Bus. Use StartNATS for everything else.
func StartNATSURL(t *testing.T) string {
	t.Helper()
	return startContainer(t, "nats", func(ctx context.Context) (string, testcontainers.Container, error) {
		ctr, err := natscontainer.Run(ctx, "nats:2.11.7")
		if err != nil {
			return "", ctr, fmt.Errorf("starting nats: %w", err)
		}
		url, err := ctr.ConnectionString(ctx)
		if err != nil {
			return "", ctr, fmt.Errorf("nats connection string: %w", err)
		}
		// The nats module only waits for the port to listen, which Docker
		// Desktop's proxy does before NATS is serving (a client then gets
		// EOF), so prove a full handshake and round trip.
		return url, ctr, waitUntilReady(ctx, "nats", readyTimeout, 200*time.Millisecond, func(ctx context.Context) error {
			nc, err := nats.Connect(url, nats.Timeout(probeAttemptTimeout), nats.NoReconnect())
			if err != nil {
				return err
			}
			defer nc.Close()
			return nc.FlushWithContext(ctx)
		})
	})
}
