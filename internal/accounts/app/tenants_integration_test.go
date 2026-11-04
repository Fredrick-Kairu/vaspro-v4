//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

func TestCreateTenant_WritesSandboxAccountAdminMembershipAndEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := uuid.New()

	tenant, err := h.svc.CreateTenant(ctx, user, domain.NewTenantInput{
		DisplayName: " Acme Deliveries ", CountryCode: "ke", LegalName: "Acme Deliveries Limited", BillingEmail: "billing@acme.example",
	})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if tenant.Type != "direct" || tenant.Tier != domain.TierSandbox || tenant.Status != "active" ||
		tenant.DisplayName != "Acme Deliveries" || tenant.CountryCode != "KE" ||
		tenant.Timezone != "Africa/Nairobi" || tenant.DefaultCurrency != "KES" || tenant.Region != "ke" {
		t.Fatalf("got %+v", tenant)
	}
	if n := h.count(t, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND user_id = $2 AND status = 'active' AND role_id = $3`,
		tenant.ID, user, domain.BuiltinAdminRoleID); n != 1 {
		t.Fatalf("got %d Admin memberships for the creator, want 1", n)
	}
	data := assertEventValid(t, h, "tenant.created", tenant.ID)
	if data["created_by"] != user.String() || data["tier"] != "sandbox" || data["region"] != "ke" {
		t.Fatalf("got event data %v", data)
	}

	t.Run("the creator can read it", func(t *testing.T) {
		got, err := h.svc.GetTenant(ctx, user, tenant.ID)
		if err != nil || got.ID != tenant.ID {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("anyone else gets not found", func(t *testing.T) {
		if _, err := h.svc.GetTenant(ctx, uuid.New(), tenant.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("an unknown tenant is not found", func(t *testing.T) {
		if _, err := h.svc.GetTenant(ctx, user, uuid.New()); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("one user can create several accounts", func(t *testing.T) {
		second, err := h.svc.CreateTenant(ctx, user, domain.NewTenantInput{DisplayName: "Acme Two", CountryCode: "KE"})
		if err != nil || second.ID == tenant.ID {
			t.Fatalf("got %+v, %v", second, err)
		}
	})
}

func TestCreateTenant_RejectedInputWritesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for name, in := range map[string]domain.NewTenantInput{
		"a country with no pack": {DisplayName: "Acme", CountryCode: "ZZ"},
		"no display name":        {DisplayName: " ", CountryCode: "KE"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.svc.CreateTenant(ctx, uuid.New(), in); !errors.Is(err, app.ErrValidation) {
				t.Fatalf("got %v, want ErrValidation", err)
			}
		})
	}
	if n := h.count(t, `SELECT count(*) FROM tenants`); n != 0 {
		t.Fatalf("%d tenants written", n)
	}
}
