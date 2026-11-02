package testkit

import (
	"context"
	"fmt"
	"time"
)

// probeAttemptTimeout bounds one probe attempt, so a connection that
// hangs (rather than failing fast) is cancelled and retried.
const probeAttemptTimeout = 2 * time.Second

// waitUntilReady calls probe until it succeeds or timeout passes. A
// container's wait strategy can pass before a client on the host can
// use it — Docker Desktop's port forward may still be down, or accept
// TCP before the server behind it is serving — so each Start* helper
// proves readiness the way its callers will connect.
func waitUntilReady(ctx context.Context, what string, timeout, interval time.Duration, probe func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	for {
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, min(probeAttemptTimeout, timeout/2))
		lastErr = probe(attemptCtx)
		cancelAttempt()
		if lastErr == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not ready after %s: %w", what, timeout, lastErr)
		case <-time.After(interval):
		}
	}
}
