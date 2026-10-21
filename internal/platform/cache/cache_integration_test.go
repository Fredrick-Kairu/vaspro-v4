//go:build integration

package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

func TestCache_SetThenGet_RoundTrips(t *testing.T) {
	ctx := context.Background()
	c := testkit.StartRedis(t)

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
	c := testkit.StartRedis(t)

	_, err := c.Get(ctx, "does-not-exist").Result()
	if err != redis.Nil {
		t.Fatalf("expected redis.Nil for a missing key, got: %v", err)
	}
}
