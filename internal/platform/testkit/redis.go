//go:build integration

package testkit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	rediscontainer "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
)

// StartRedis starts a Redis container and returns a connected Cache.
// Cleans itself up via t.Cleanup.
func StartRedis(t *testing.T) *cache.Cache {
	t.Helper()
	uri := StartRedisURL(t)

	opts, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatalf("testkit: failed to parse redis url %q: %v", uri, err)
	}

	c := cache.New(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// StartRedisURL starts a Redis container and returns its connection
// URL, for a caller that needs to hand it to a separate process (e.g.
// a built service binary under test, or identity.Config.RedisURL) —
// rather than an in-process *cache.Cache. Use StartRedis for everything
// else.
func StartRedisURL(t *testing.T) string {
	t.Helper()
	return startContainer(t, "redis", func(ctx context.Context) (string, testcontainers.Container, error) {
		// The module's own port wait gives up after 10s, which a loaded
		// Docker daemon (a full parallel integration run) can exceed; use
		// the same 60s budget as postgres.
		ctr, err := rediscontainer.Run(ctx, "redis:7-alpine",
			testcontainers.WithWaitStrategy(wait.ForAll(
				wait.ForListeningPort("6379/tcp"),
				wait.ForLog("Ready to accept connections"),
			).WithDeadline(60*time.Second)),
		)
		if err != nil {
			return "", ctr, fmt.Errorf("starting redis: %w", err)
		}
		uri, err := ctr.ConnectionString(ctx)
		if err != nil {
			return "", ctr, fmt.Errorf("redis connection string: %w", err)
		}
		opts, err := redis.ParseURL(uri)
		if err != nil {
			return "", ctr, fmt.Errorf("parsing redis url %q: %w", uri, err)
		}
		return uri, ctr, waitUntilReady(ctx, "redis", readyTimeout, 200*time.Millisecond, func(ctx context.Context) error {
			c := redis.NewClient(opts)
			defer func() { _ = c.Close() }()
			return c.Ping(ctx).Err()
		})
	})
}
