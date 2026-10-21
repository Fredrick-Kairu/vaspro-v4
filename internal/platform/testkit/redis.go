//go:build integration

package testkit

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	rediscontainer "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
)

// StartRedis starts a Redis container and returns a connected Cache.
// Cleans itself up via t.Cleanup.
func StartRedis(t *testing.T) *cache.Cache {
	t.Helper()
	ctx := context.Background()

	ctr, err := rediscontainer.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("testkit: failed to start redis container: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)

	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("testkit: failed to get redis connection string: %v", err)
	}

	opts, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatalf("testkit: failed to parse redis url %q: %v", uri, err)
	}

	c := cache.New(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}
