//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantaccess"
)

// signIn signs a new user in and returns their user and session IDs.
func signIn(t *testing.T, ctx context.Context, svc *app.Service, sender *email.Sender, emailAddr string) (userID, sessionID uuid.UUID) {
	t.Helper()
	setPassword(t, ctx, svc, sender, emailAddr, "a-strong-password")
	result, err := svc.SignIn(ctx, emailAddr, "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	sessionID, userID, err = svc.ResolveSession(ctx, result.Session.Token)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	return userID, sessionID
}

func sessionTenantColumn(t *testing.T, pool *vasdb.Pool, sessionID uuid.UUID) *uuid.UUID {
	t.Helper()
	var tenant *uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT tenant_id FROM sessions WHERE id = $1`, sessionID).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	return tenant
}

func TestSelectSessionTenant(t *testing.T) {
	access := &tenantaccess.Fake{}
	svc, pool, sender, _, _ := newTestServiceWith(t, nil, access)
	ctx := context.Background()
	ada, adaSession := signIn(t, ctx, svc, sender, "ada@example.com")
	_, bobSession := signIn(t, ctx, svc, sender, "bob@example.com")
	tenant, other := uuid.New(), uuid.New()
	access.Grant(ada, tenant)

	t.Run("an account the user can't reach is not found, and nothing is set", func(t *testing.T) {
		if err := svc.SelectSessionTenant(ctx, ada, adaSession, other); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
		if got := sessionTenantColumn(t, pool, adaSession); got != nil {
			t.Fatalf("tenant_id set to %s", got)
		}
	})
	t.Run("another user's session is not found", func(t *testing.T) {
		if err := svc.SelectSessionTenant(ctx, ada, bobSession, tenant); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
		if got := sessionTenantColumn(t, pool, bobSession); got != nil {
			t.Fatalf("bob's session got tenant_id %s", got)
		}
	})
	t.Run("an account the user belongs to is selected", func(t *testing.T) {
		if err := svc.SelectSessionTenant(ctx, ada, adaSession, tenant); err != nil {
			t.Fatalf("SelectSessionTenant: %v", err)
		}
		if got := sessionTenantColumn(t, pool, adaSession); got == nil || *got != tenant {
			t.Fatalf("tenant_id %v, want %s", got, tenant)
		}
	})
	t.Run("a revoked session can't select", func(t *testing.T) {
		if err := svc.RevokeSession(ctx, ada, adaSession); err != nil {
			t.Fatal(err)
		}
		if err := svc.SelectSessionTenant(ctx, ada, adaSession, tenant); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
}

func TestSessionTenant(t *testing.T) {
	access := &tenantaccess.Fake{}
	svc, _, sender, _, _ := newTestServiceWith(t, nil, access)
	ctx := context.Background()
	ada, session := signIn(t, ctx, svc, sender, "ada@example.com")
	tenant := uuid.New()

	if _, err := svc.SessionTenant(ctx, ada, session, ""); !errors.Is(err, app.ErrNoTenantSelected) {
		t.Fatalf("before selecting: got %v, want ErrNoTenantSelected", err)
	}

	access.Grant(ada, tenant)
	if err := svc.SelectSessionTenant(ctx, ada, session, tenant); err != nil {
		t.Fatal(err)
	}
	got, err := svc.SessionTenant(ctx, ada, session, "")
	if err != nil || got != tenant {
		t.Fatalf("after selecting: got %s, %v; want %s", got, err, tenant)
	}

	t.Run("an accounts failure fails closed", func(t *testing.T) {
		boom := errors.New("accounts unreachable")
		access.FailWith(boom)
		defer access.FailWith(nil)
		if _, err := svc.SessionTenant(ctx, ada, session, ""); !errors.Is(err, boom) || errors.Is(err, app.ErrNoTenantSelected) {
			t.Fatalf("got %v, want the accounts error", err)
		}
	})
	t.Run("removing the member takes effect at once", func(t *testing.T) {
		access.Revoke(ada, tenant)
		if _, err := svc.SessionTenant(ctx, ada, session, ""); !errors.Is(err, app.ErrNoTenantSelected) {
			t.Fatalf("got %v, want ErrNoTenantSelected", err)
		}
	})
}

func TestSessionTenant_Permission(t *testing.T) {
	access := &tenantaccess.Fake{}
	svc, _, sender, _, _ := newTestServiceWith(t, nil, access)
	ctx := context.Background()
	ada, session := signIn(t, ctx, svc, sender, "ada@example.com")
	tenant := uuid.New()
	access.GrantOnly(ada, tenant, "credentials:read")
	if err := svc.SelectSessionTenant(ctx, ada, session, tenant); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.SessionTenant(ctx, ada, session, "credentials:read"); err != nil || got != tenant {
		t.Fatalf("a permission the role has: got %s, %v", got, err)
	}
	if _, err := svc.SessionTenant(ctx, ada, session, "credentials:manage"); !errors.Is(err, app.ErrPermissionDenied) {
		t.Fatalf("a permission the role lacks: got %v, want ErrPermissionDenied", err)
	}
}
