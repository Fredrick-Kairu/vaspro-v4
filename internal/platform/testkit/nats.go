//go:build integration

package testkit

import (
	"context"
	"testing"

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
	ctx := context.Background()

	ctr, err := natscontainer.Run(ctx, "nats:2.11.7")
	if err != nil {
		t.Fatalf("testkit: failed to start nats container: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)

	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("testkit: failed to get nats connection string: %v", err)
	}
	return url
}
