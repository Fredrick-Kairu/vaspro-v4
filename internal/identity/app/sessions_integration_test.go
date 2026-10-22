//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

func TestResolveSession_CacheMissOnFirstUse_ThenPopulatesCache(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, c := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	key := "identity:session:" + result.Session.ID.String()

	// Cache is populated lazily, not write-through at sign-in — a fresh
	// session is a guaranteed miss until the first ResolveSession call.
	if _, err := c.Get(ctx, key).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expected a cache miss right after sign-in, got err=%v", err)
	}

	userID, err := svc.ResolveSession(ctx, result.Session.ID)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}

	cached, err := c.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("expected the cache to be populated after ResolveSession, got err=%v", err)
	}
	if cached != userID.String() {
		t.Fatalf("cached value %q, want %q", cached, userID.String())
	}
}

func TestResolveSession_CacheMiss_FallsBackToPostgresAndBumpsLastSeenAt(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, c := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	before, err := store.New(pool).GetSessionByID(ctx, result.Session.ID)
	if err != nil {
		t.Fatalf("GetSessionByID: %v", err)
	}

	// Force a cache miss even though ResolveSession might otherwise have
	// populated it already.
	key := "identity:session:" + result.Session.ID.String()
	if err := c.Del(ctx, key).Err(); err != nil {
		t.Fatalf("Del: %v", err)
	}

	if _, err := svc.ResolveSession(ctx, result.Session.ID); err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}

	after, err := store.New(pool).GetSessionByID(ctx, result.Session.ID)
	if err != nil {
		t.Fatalf("GetSessionByID (after): %v", err)
	}
	if !after.LastSeenAt.After(before.LastSeenAt) {
		t.Fatalf("expected last_seen_at to advance past %v, got %v", before.LastSeenAt, after.LastSeenAt)
	}
}

func TestListSessions_ShowsActiveSessionsWithCurrentFlag(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	first, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "first-agent")
	if err != nil {
		t.Fatalf("first SignIn: %v", err)
	}
	second, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "second-agent")
	if err != nil {
		t.Fatalf("second SignIn: %v", err)
	}

	userID, err := svc.ResolveSession(ctx, first.Session.ID)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}

	sessions, err := svc.ListSessions(ctx, userID, second.Session.ID)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}

	var sawCurrent, sawOther bool
	for _, s := range sessions {
		switch s.ID {
		case second.Session.ID:
			if !s.Current {
				t.Fatal("expected the second session to be flagged current")
			}
			sawCurrent = true
		case first.Session.ID:
			if s.Current {
				t.Fatal("expected the first session to not be flagged current")
			}
			sawOther = true
		}
	}
	if !sawCurrent || !sawOther {
		t.Fatalf("expected both sessions in the list, got %+v", sessions)
	}
}

func TestRevokeSession_InvalidatesCacheAndPostgres(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, c := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	userID, err := svc.ResolveSession(ctx, result.Session.ID)
	if err != nil {
		t.Fatalf("priming ResolveSession: %v", err)
	}

	if err := svc.RevokeSession(ctx, userID, result.Session.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	key := "identity:session:" + result.Session.ID.String()
	if _, err := c.Get(ctx, key).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expected the cache entry to be evicted, got err=%v", err)
	}

	if _, err := svc.ResolveSession(ctx, result.Session.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("got error %v, want app.ErrNotFound after revocation", err)
	}
}

func TestRevokeSession_NotOwned_ReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")
	setPassword(t, ctx, svc, sender, "bob@example.com", "a-strong-password")
	bob, err := store.New(pool).GetUserByEmail(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail(bob): %v", err)
	}

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	if err := svc.RevokeSession(ctx, bob.ID, result.Session.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("got error %v, want app.ErrNotFound", err)
	}
}

func TestSignOutEverywhere_RevokesEverySessionIncludingCurrent(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	first, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "first-agent")
	if err != nil {
		t.Fatalf("first SignIn: %v", err)
	}
	second, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "second-agent")
	if err != nil {
		t.Fatalf("second SignIn: %v", err)
	}
	userID, err := svc.ResolveSession(ctx, first.Session.ID)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}

	count, err := svc.SignOutEverywhere(ctx, userID)
	if err != nil {
		t.Fatalf("SignOutEverywhere: %v", err)
	}
	if count != 2 {
		t.Fatalf("got %d sessions revoked, want 2", count)
	}

	if _, err := svc.ResolveSession(ctx, first.Session.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("got error %v for the first (current) session, want app.ErrNotFound", err)
	}
	if _, err := svc.ResolveSession(ctx, second.Session.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("got error %v for the second session, want app.ErrNotFound", err)
	}
}
