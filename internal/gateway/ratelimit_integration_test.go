//go:build integration

package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

func TestRedisRateLimiter_AllowsUpToLimitThenDenies(t *testing.T) {
	ctx := context.Background()
	c := testkit.StartRedis(t)
	limiter := NewRedisRateLimiter(c)

	for i := 0; i < 3; i++ {
		allowed, _, err := limiter.Allow(ctx, "test-key", 3, time.Minute)
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if !allowed {
			t.Fatalf("request %d: got denied, want allowed", i)
		}
	}

	allowed, retryAfter, err := limiter.Allow(ctx, "test-key", 3, time.Minute)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if allowed {
		t.Fatal("expected the 4th request to be denied")
	}
	if retryAfter <= 0 {
		t.Fatalf("got retryAfter %v, want a positive duration", retryAfter)
	}
}

func TestRedisRateLimiter_DifferentKeysAreIndependent(t *testing.T) {
	ctx := context.Background()
	c := testkit.StartRedis(t)
	limiter := NewRedisRateLimiter(c)

	allowedA, _, err := limiter.Allow(ctx, "key-a", 1, time.Minute)
	if err != nil {
		t.Fatalf("Allow(key-a): %v", err)
	}
	if !allowedA {
		t.Fatal("expected key-a's first request to be allowed")
	}

	allowedB, _, err := limiter.Allow(ctx, "key-b", 1, time.Minute)
	if err != nil {
		t.Fatalf("Allow(key-b): %v", err)
	}
	if !allowedB {
		t.Fatal("expected key-b's first request to be allowed independently of key-a")
	}
}
