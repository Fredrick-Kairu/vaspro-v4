//go:build integration

package app_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
)

func TestEnsureActiveSigningKey_CreatesOnlyWhenNoneExists(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newTestService(t)

	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey (bootstrap): %v", err)
	}
	kid1, _, err := svc.ActiveSigningKey(ctx)
	if err != nil {
		t.Fatalf("ActiveSigningKey: %v", err)
	}
	if kid1 == "" {
		t.Fatal("expected a non-empty kid after bootstrapping")
	}

	// A no-op second call must not replace the key that already exists.
	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey (no-op): %v", err)
	}
	kid2, _, err := svc.ActiveSigningKey(ctx)
	if err != nil {
		t.Fatalf("ActiveSigningKey after no-op: %v", err)
	}
	if kid1 != kid2 {
		t.Fatalf("got a different active kid after a no-op EnsureActiveSigningKey: %s vs %s", kid1, kid2)
	}
}

func TestActiveSigningKey_RoundTripsThePublicKey(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newTestService(t)

	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}
	kid, priv, err := svc.ActiveSigningKey(ctx)
	if err != nil {
		t.Fatalf("ActiveSigningKey: %v", err)
	}

	keys, err := svc.PublishableSigningKeys(ctx)
	if err != nil {
		t.Fatalf("PublishableSigningKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Kid != kid {
		t.Fatalf("got %v, want exactly one key with kid %s", keys, kid)
	}
	if !bytes.Equal(keys[0].PublicKey, priv.Public().(ed25519.PublicKey)) {
		t.Fatal("published public key doesn't match the active key's own private key's public half")
	}
}

func TestRotateSigningKey_DemotesActivatesAndRetiresStale(t *testing.T) {
	ctx := context.Background()
	svc, _, _, clk, _ := newTestService(t)

	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}
	firstKid, _, err := svc.ActiveSigningKey(ctx)
	if err != nil {
		t.Fatalf("ActiveSigningKey: %v", err)
	}

	if err := svc.RotateSigningKey(ctx); err != nil {
		t.Fatalf("RotateSigningKey (first): %v", err)
	}
	secondKid, _, err := svc.ActiveSigningKey(ctx)
	if err != nil {
		t.Fatalf("ActiveSigningKey after first rotation: %v", err)
	}
	if secondKid == firstKid {
		t.Fatal("expected a fresh active kid after rotation")
	}

	// Still within the 24h overlap: the demoted key stays published.
	keys, err := svc.PublishableSigningKeys(ctx)
	if err != nil {
		t.Fatalf("PublishableSigningKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d publishable keys, want 2 (active + retiring)", len(keys))
	}

	// Past the overlap: the next rotation's sweep retires firstKid.
	clk.Advance(25 * time.Hour)
	if err := svc.RotateSigningKey(ctx); err != nil {
		t.Fatalf("RotateSigningKey (second): %v", err)
	}
	if _, _, err := svc.ActiveSigningKey(ctx); err != nil {
		t.Fatalf("ActiveSigningKey after second rotation: %v", err)
	}

	keys, err = svc.PublishableSigningKeys(ctx)
	if err != nil {
		t.Fatalf("PublishableSigningKeys after sweep: %v", err)
	}
	var publishedKids []string
	for _, k := range keys {
		publishedKids = append(publishedKids, k.Kid)
	}
	if len(publishedKids) != 2 {
		t.Fatalf("got published kids %v, want exactly 2 (secondKid + the new one; firstKid swept to retired)", publishedKids)
	}
	for _, kid := range publishedKids {
		if kid == firstKid {
			t.Fatalf("expected firstKid (%s) to have been retired by the sweep, still published: %v", firstKid, publishedKids)
		}
	}
}

func TestPublishableSigningKeys_PublishesOnlyActiveAlgorithm(t *testing.T) {
	// Every key this service ever generates uses domain.SigningKeyAlgorithm
	// ("EdDSA") — a cheap sanity check that PublishableSigningKeys passes
	// the stored algorithm through rather than hardcoding something else.
	ctx := context.Background()
	svc, _, _, _, _ := newTestService(t)

	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}
	keys, err := svc.PublishableSigningKeys(ctx)
	if err != nil {
		t.Fatalf("PublishableSigningKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Algorithm != domain.SigningKeyAlgorithm {
		t.Fatalf("got %v, want one key with algorithm %s", keys, domain.SigningKeyAlgorithm)
	}
}
