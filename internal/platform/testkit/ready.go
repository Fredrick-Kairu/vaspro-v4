package testkit

import (
	"context"
	"fmt"
	"time"
)

// probeAttemptTimeout bounds one probe attempt, so a connection that
// hangs (rather than failing fast) is cancelled and retried. A fresh
// Postgres under a full integration run can take more than 2s to answer
// a connection it has accepted (seen 2026-11-03), so attempts get 5s.
const probeAttemptTimeout = 5 * time.Second

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

// startWithRetries runs start until it succeeds, up to attempts times.
// start returns what the caller needs and how to tear its container
// down; a failed attempt's container is torn down before the next one
// starts, and each retry is logged. After the last attempt the error
// names what failed and every attempt's count.
func startWithRetries[T any](ctx context.Context, logf func(string, ...any), what string, attempts int, start func(context.Context) (T, func(), error)) (T, func(), error) {
	var zero T
	for attempt := 1; ; attempt++ {
		value, terminate, err := start(ctx)
		if err == nil {
			return value, terminate, nil
		}
		if terminate != nil {
			terminate()
		}
		if attempt >= attempts {
			return zero, nil, fmt.Errorf("%s didn't become ready after %d attempts: %w", what, attempt, err)
		}
		logf("testkit: %s attempt %d failed, starting a fresh container: %v", what, attempt, err)
	}
}
