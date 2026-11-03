//go:build integration

package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

func securityEventTypes(t *testing.T, ctx context.Context, q *store.Queries, userID uuid.UUID) map[string]int {
	t.Helper()
	events, err := q.ListSecurityEventsByUser(ctx, pgtype.UUID{Bytes: userID, Valid: true})
	if err != nil {
		t.Fatalf("ListSecurityEventsByUser: %v", err)
	}
	out := map[string]int{}
	for _, e := range events {
		out[e.Type]++
	}
	return out
}

func TestStaffMembers_GrantRevokeIsStaff(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)
	q := store.New(pool)
	ada := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	if staff, err := svc.IsStaff(ctx, ada); err != nil || staff {
		t.Fatalf("before granting: got %v, %v; want false", staff, err)
	}
	if err := svc.GrantStaff(ctx, "ADA@example.com", "ops-on-call"); err != nil {
		t.Fatalf("GrantStaff: %v", err)
	}
	if staff, err := svc.IsStaff(ctx, ada); err != nil || !staff {
		t.Fatalf("after granting: got %v, %v; want true", staff, err)
	}
	if err := svc.GrantStaff(ctx, "ada@example.com", "ops-on-call"); err != nil {
		t.Fatalf("granting again should change nothing, got %v", err)
	}
	if got := securityEventTypes(t, ctx, q, ada)["staff_granted"]; got != 1 {
		t.Fatalf("%d staff_granted events, want 1 (a repeat grant changes nothing)", got)
	}

	if err := svc.RevokeStaff(ctx, "ada@example.com", "ops-on-call"); err != nil {
		t.Fatalf("RevokeStaff: %v", err)
	}
	if staff, err := svc.IsStaff(ctx, ada); err != nil || staff {
		t.Fatalf("after revoking: got %v, %v; want false", staff, err)
	}
	if got := securityEventTypes(t, ctx, q, ada)["staff_revoked"]; got != 1 {
		t.Fatalf("%d staff_revoked events, want 1", got)
	}

	if staff, err := svc.IsStaff(ctx, uuid.New()); err != nil || staff {
		t.Fatalf("unknown user: got %v, %v; want false", staff, err)
	}
	for name, call := range map[string]func() error{
		"grant unknown":  func() error { return svc.GrantStaff(ctx, "nobody@example.com", "ops") },
		"revoke unknown": func() error { return svc.RevokeStaff(ctx, "nobody@example.com", "ops") },
		"revoke non-staff": func() error {
			return svc.RevokeStaff(ctx, "ada@example.com", "ops")
		},
	} {
		if err := call(); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("%s: got %v, want ErrNotFound", name, err)
		}
	}
	if err := svc.GrantStaff(ctx, "ada@example.com", " "); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("no operator name: got %v, want ErrValidation", err)
	}
}

func TestUnlockByStaff(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)
	q := store.New(pool)
	staff := signedInUserID(t, ctx, svc, sender, "staff@example.com")
	if err := svc.GrantStaff(ctx, "staff@example.com", "ops"); err != nil {
		t.Fatalf("GrantStaff: %v", err)
	}
	stranger := signedInUserID(t, ctx, svc, sender, "stranger@example.com")
	setPassword(t, ctx, svc, sender, "locked@example.com", "a-strong-password")
	lockOut := func() {
		t.Helper()
		for i := 0; i < 5; i++ {
			if _, err := svc.SignIn(ctx, "locked@example.com", "wrong-password", nil, "test-agent"); !errors.Is(err, app.ErrBadCredentials) {
				t.Fatalf("attempt %d: got %v", i, err)
			}
		}
		if _, err := svc.SignIn(ctx, "locked@example.com", "a-strong-password", nil, "test-agent"); !errors.Is(err, app.ErrBadCredentials) {
			t.Fatalf("expected a lockout, got %v", err)
		}
	}
	lockOut()
	locked, err := q.GetUserByEmail(ctx, "locked@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	t.Run("refusals write nothing", func(t *testing.T) {
		if err := svc.UnlockByStaff(ctx, stranger, "locked@example.com"); !errors.Is(err, app.ErrNotStaff) {
			t.Fatalf("a non-staff caller: got %v, want ErrNotStaff", err)
		}
		if err := svc.UnlockByStaff(ctx, staff, "nobody@example.com"); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("unknown email: got %v, want ErrNotFound", err)
		}
		if err := svc.UnlockByStaff(ctx, staff, "not an email"); !errors.Is(err, app.ErrNotFound) && !errors.Is(err, app.ErrValidation) {
			t.Fatalf("malformed email: got %v", err)
		}
		if got := securityEventTypes(t, ctx, q, locked.ID)["unlocked_by_staff"]; got != 0 {
			t.Fatalf("%d unlock events after refusals, want 0", got)
		}
		if _, err := svc.SignIn(ctx, "locked@example.com", "a-strong-password", nil, "test-agent"); !errors.Is(err, app.ErrBadCredentials) {
			t.Fatalf("still locked expected, got %v", err)
		}
	})

	t.Run("staff unlock lets the user sign in", func(t *testing.T) {
		if err := svc.UnlockByStaff(ctx, staff, "Locked@Example.com"); err != nil {
			t.Fatalf("UnlockByStaff: %v", err)
		}
		if _, err := svc.SignIn(ctx, "locked@example.com", "a-strong-password", nil, "test-agent"); err != nil {
			t.Fatalf("after the unlock: %v", err)
		}
		events, err := q.ListSecurityEventsByUser(ctx, pgtype.UUID{Bytes: locked.ID, Valid: true})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range events {
			if e.Type == "unlocked_by_staff" {
				found = strings.Contains(string(e.Detail), staff.String())
			}
		}
		if !found {
			t.Fatal("no unlocked_by_staff event naming the staff member")
		}
	})

	t.Run("new failures after the unlock lock again", func(t *testing.T) {
		lockOut()
	})

	t.Run("a revoked staff member can't unlock", func(t *testing.T) {
		if err := svc.RevokeStaff(ctx, "staff@example.com", "ops"); err != nil {
			t.Fatal(err)
		}
		if err := svc.UnlockByStaff(ctx, staff, "locked@example.com"); !errors.Is(err, app.ErrNotStaff) {
			t.Fatalf("got %v, want ErrNotStaff", err)
		}
	})
}
