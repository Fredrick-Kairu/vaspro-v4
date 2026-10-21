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

	b, err := bus.Connect(ctx, url)
	if err != nil {
		t.Fatalf("testkit: failed to connect to nats: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}
