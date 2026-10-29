//go:build integration

package hmacauth_test

import (
	"context"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/hmacauth"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// TestRedisNonceStore_ClaimOnceOnly is the real proof Redis's SETNX is
// doing the atomic claim-once work — hmacauth_test.go's fakeNonceStore
// (a plain Go map) can't exercise that, only the unit-level behavior
// the middleware expects from any NonceStore.
func TestRedisNonceStore_ClaimOnceOnly(t *testing.T) {
	ctx := context.Background()
	c := testkit.StartRedis(t)
	store := hmacauth.NewRedisNonceStore(c)

	claimed, err := store.Claim(ctx, "nonce-1", time.Minute)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected the first Claim of a fresh nonce to succeed")
	}

	claimed, err = store.Claim(ctx, "nonce-1", time.Minute)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if claimed {
		t.Fatal("expected the second Claim of the same nonce to be rejected as a replay")
	}
}

func TestRedisNonceStore_DifferentNoncesAreIndependent(t *testing.T) {
	ctx := context.Background()
	c := testkit.StartRedis(t)
	store := hmacauth.NewRedisNonceStore(c)

	for _, nonce := range []string{"nonce-a", "nonce-b", "nonce-c"} {
		claimed, err := store.Claim(ctx, nonce, time.Minute)
		if err != nil {
			t.Fatalf("Claim(%q): %v", nonce, err)
		}
		if !claimed {
			t.Fatalf("expected Claim(%q) to succeed as a fresh nonce", nonce)
		}
	}
}
