//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

var acmeProfile = domain.BusinessProfileInput{
	RegistrationNumber: "pvt-ab1cdef2", RegisteredName: "Acme Deliveries Limited", TaxID: "p051234567x", RegisteredAddress: "Moi Avenue, Nairobi",
}

// starterTeam is newTeam promoted to starter by staff.
func (h harness) starterTeam(t *testing.T) (tenant, admin, viewer uuid.UUID) {
	t.Helper()
	tenant, admin, _, viewer = h.newTeam(t)
	if err := h.svc.ChangeTier(context.Background(), app.ChangeTierRequest{
		TenantID: tenant, To: domain.TierStarter, Reason: "kyc", ChangedBy: ptr(uuid.New()),
	}); err != nil {
		t.Fatal(err)
	}
	return tenant, admin, viewer
}

func TestBusinessProfileAndOwners(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, admin, _, viewer := h.newTeam(t)

	if _, err := h.svc.GetBusinessProfile(ctx, admin, tenant); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("no profile yet: got %v, want ErrNotFound", err)
	}
	if _, err := h.svc.AddOwner(ctx, admin, tenant, domain.OwnerInput{FullName: "Grace", Role: domain.OwnerDirector}); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("an owner before the profile: got %v, want ErrNotFound", err)
	}
	p, err := h.svc.SetBusinessProfile(ctx, admin, tenant, acmeProfile)
	if err != nil || p.Registry != "BRS" || p.RegistrationNumber != "PVT-AB1CDEF2" || p.TaxID != "P051234567X" {
		t.Fatalf("SetBusinessProfile: %+v, %v; want the pack's registry and normalised fields", p, err)
	}
	updated := acmeProfile
	updated.RegisteredAddress = "Kenyatta Avenue, Nairobi"
	if p, err := h.svc.SetBusinessProfile(ctx, admin, tenant, updated); err != nil || p.RegisteredAddress != "Kenyatta Avenue, Nairobi" {
		t.Fatalf("updating: %+v, %v", p, err)
	}
	if n := h.count(t, `SELECT count(*) FROM business_profiles WHERE tenant_id = $1`, tenant); n != 1 {
		t.Fatalf("%d profiles, want one per account", n)
	}

	grace, err := h.svc.AddOwner(ctx, admin, tenant, domain.OwnerInput{FullName: "Grace Wanjiru", Role: domain.OwnerDirector, OwnershipPct: "60.5"})
	if err != nil || grace.OwnershipPct != "60.50" {
		t.Fatalf("AddOwner: %+v, %v", grace, err)
	}
	brian, err := h.svc.AddOwner(ctx, admin, tenant, domain.OwnerInput{FullName: "Brian Otieno", Role: domain.OwnerSignatory})
	if err != nil || brian.OwnershipPct != "" {
		t.Fatalf("AddOwner without a percentage: %+v, %v", brian, err)
	}
	got, err := h.svc.GetBusinessProfile(ctx, viewer, tenant)
	if err != nil || len(got.Owners) != 2 {
		t.Fatalf("a Viewer reading the profile: %+v, %v", got, err)
	}
	if err := h.svc.RemoveOwner(ctx, admin, tenant, brian.ID); err != nil {
		t.Fatal(err)
	}

	t.Run("a Viewer can't edit", func(t *testing.T) {
		if _, err := h.svc.SetBusinessProfile(ctx, viewer, tenant, acmeProfile); !errors.Is(err, domain.ErrPermissionDenied) {
			t.Fatalf("got %v", err)
		}
		if _, err := h.svc.AddOwner(ctx, viewer, tenant, domain.OwnerInput{FullName: "X", Role: domain.OwnerOwner}); !errors.Is(err, domain.ErrPermissionDenied) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("bad input", func(t *testing.T) {
		if _, err := h.svc.SetBusinessProfile(ctx, admin, tenant, domain.BusinessProfileInput{RegisteredName: "No number"}); !errors.Is(err, app.ErrValidation) {
			t.Fatalf("got %v", err)
		}
		if _, err := h.svc.AddOwner(ctx, admin, tenant, domain.OwnerInput{FullName: "X", Role: domain.OwnerOwner, OwnershipPct: "101"}); !errors.Is(err, app.ErrValidation) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("another account's owner is not found", func(t *testing.T) {
		other, otherAdmin, _, _ := h.newTeam(t)
		if _, err := h.svc.SetBusinessProfile(ctx, otherAdmin, other, acmeProfile); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.RemoveOwner(ctx, otherAdmin, other, grace.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
}

func TestSubmitKYB(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	t.Run("refusals", func(t *testing.T) {
		sandboxTenant, sandboxAdmin, _, _ := h.newTeam(t)
		if _, err := h.svc.SubmitKYB(ctx, sandboxAdmin, sandboxTenant); !errors.Is(err, domain.ErrKYBNeedsStarter) {
			t.Fatalf("in the sandbox: got %v, want ErrKYBNeedsStarter", err)
		}
		tenant, admin, viewer := h.starterTeam(t)
		if _, err := h.svc.SubmitKYB(ctx, admin, tenant); !errors.Is(err, domain.ErrNoBusinessProfile) {
			t.Fatalf("no profile: got %v", err)
		}
		ownersOnly := acmeProfile
		ownersOnly.RegistrationNumber = "PVT-REFUSALS1"
		if _, err := h.svc.SetBusinessProfile(ctx, admin, tenant, ownersOnly); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.AddOwner(ctx, admin, tenant, domain.OwnerInput{FullName: "Owner Only", Role: domain.OwnerOwner}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.SubmitKYB(ctx, admin, tenant); !errors.Is(err, domain.ErrNoDirector) {
			t.Fatalf("no director: got %v", err)
		}
		if _, err := h.svc.SubmitKYB(ctx, viewer, tenant); !errors.Is(err, domain.ErrPermissionDenied) {
			t.Fatalf("a Viewer: got %v", err)
		}
		if n := h.count(t, `SELECT count(*) FROM kyc_checks WHERE kind = 'business'`); n != 0 {
			t.Fatalf("%d business checks written by refusals", n)
		}
	})

	// Another account already uses the same registration number.
	elsewhere, elsewhereAdmin, _ := h.starterTeam(t)
	if _, err := h.svc.SetBusinessProfile(ctx, elsewhereAdmin, elsewhere, acmeProfile); err != nil {
		t.Fatal(err)
	}

	tenant, admin, _ := h.starterTeam(t)
	profile, err := h.svc.SetBusinessProfile(ctx, admin, tenant, acmeProfile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AddOwner(ctx, admin, tenant, domain.OwnerInput{FullName: "Grace Wanjiru", Role: domain.OwnerDirector}); err != nil {
		t.Fatal(err)
	}

	check, err := h.svc.SubmitKYB(ctx, admin, tenant)
	if err != nil || check.Kind != domain.KYCKindBusiness || check.Status != domain.KYCReview {
		t.Fatalf("SubmitKYB: %+v, %v; want a business check in review", check, err)
	}
	var subject uuid.UUID
	var provider string
	var result []byte
	if err := h.super.QueryRow(ctx, `SELECT subject_id, provider, result FROM kyc_checks WHERE id = $1`, check.ID).Scan(&subject, &provider, &result); err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	_ = json.Unmarshal(result, &summary)
	if subject != profile.ID || provider != "manual" || summary["duplicate_registration"] != true || summary["other_accounts"] != float64(1) {
		t.Fatalf("subject %s, provider %s, result %v; want the profile, manual, and the duplicate flagged", subject, provider, summary)
	}
	if data := assertEventValid(t, h, "kyc.check_started", tenant); data["kind"] != "business" || data["provider"] != "manual" {
		t.Fatalf("got event data %v", data)
	}
	if got := h.tierOf(t, tenant); got != "starter" {
		t.Fatalf("tier %s: a submission alone never promotes", got)
	}

	t.Run("while in review, everything is locked", func(t *testing.T) {
		if _, err := h.svc.SubmitKYB(ctx, admin, tenant); !errors.Is(err, domain.ErrBusinessProfileLocked) {
			t.Fatalf("submitting again: got %v", err)
		}
		if _, err := h.svc.SetBusinessProfile(ctx, admin, tenant, acmeProfile); !errors.Is(err, domain.ErrBusinessProfileLocked) {
			t.Fatalf("editing the profile: got %v", err)
		}
		if _, err := h.svc.AddOwner(ctx, admin, tenant, domain.OwnerInput{FullName: "New", Role: domain.OwnerDirector}); !errors.Is(err, domain.ErrBusinessProfileLocked) {
			t.Fatalf("adding an owner: got %v", err)
		}
	})
	t.Run("a unique registration number isn't flagged", func(t *testing.T) {
		other, otherAdmin, _ := h.starterTeam(t)
		unique := acmeProfile
		unique.RegistrationNumber = "PVT-UNIQUE1"
		if _, err := h.svc.SetBusinessProfile(ctx, otherAdmin, other, unique); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.AddOwner(ctx, otherAdmin, other, domain.OwnerInput{FullName: "D", Role: domain.OwnerDirector}); err != nil {
			t.Fatal(err)
		}
		c, err := h.svc.SubmitKYB(ctx, otherAdmin, other)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.super.QueryRow(ctx, `SELECT result FROM kyc_checks WHERE id = $1`, c.ID).Scan(&result); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(result, &summary)
		if summary["duplicate_registration"] != false {
			t.Fatalf("result %v", summary)
		}
	})
	t.Run("a passed business check keeps it locked", func(t *testing.T) {
		if _, err := h.super.Exec(ctx, `UPDATE kyc_checks SET status = 'passed', decided_at = now() WHERE id = $1`, check.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.SetBusinessProfile(ctx, admin, tenant, acmeProfile); !errors.Is(err, domain.ErrBusinessProfileLocked) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("after a failed check it can be edited and submitted again", func(t *testing.T) {
		if _, err := h.super.Exec(ctx, `UPDATE kyc_checks SET status = 'failed' WHERE id = $1`, check.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.SetBusinessProfile(ctx, admin, tenant, acmeProfile); err != nil {
			t.Fatalf("editing after a failure: %v", err)
		}
		if _, err := h.svc.SubmitKYB(ctx, admin, tenant); err != nil {
			t.Fatalf("submitting again after a failure: %v", err)
		}
	})
}
