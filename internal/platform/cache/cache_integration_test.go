//go:build integration

package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	rediscontainer "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
)

func newTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	ctx := context.Background()

	ctr, err := rediscontainer.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("failed to start redis container: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)

	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}

	opts, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatalf("failed to parse redis url %q: %v", uri, err)
	}

	c := cache.New(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCache_SetThenGet_RoundTrips(t *testing.T) {
	ctx := context.Background()
	c := newTestCache(t)

	if err := c.Set(ctx, "key-1", "value-1", time.Minute).Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := c.Get(ctx, "key-1").Result()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "value-1" {
		t.Fatalf("got %q, want %q", got, "value-1")
	}
}

func TestCache_GetMissingKey_ReturnsRedisNil(t *testing.T) {
	ctx := context.Background()
	c := newTestCache(t)

	_, err := c.Get(ctx, "does-not-exist").Result()
	if err != redis.Nil {
		t.Fatalf("expected redis.Nil for a missing key, got: %v", err)
	}
}
