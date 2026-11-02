//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/kycvendor"
)

// deliver hands the fake vendor's webhook for ref to accounts, as the
// webhook route does.
func (h harness) deliver(t *testing.T, ref, status string, tamper kycvendor.Tamper) error {
	t.Helper()
	header, body := h.vendor.Webhook(ref, status, tamper)
	return h.svc.HandleKYCWebhook(context.Background(), "fake", header, body)
}

func (h harness) providerRef(t *testing.T, checkID uuid.UUID) string {
	t.Helper()
	var ref string
	if err := h.super.QueryRow(context.Background(), `SELECT provider_ref FROM kyc_checks WHERE id = $1`, checkID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	return ref
}

func (h harness) checkStatus(t *testing.T, checkID uuid.UUID) string {
	t.Helper()
	var status string
	if err := h.super.QueryRow(context.Background(), `SELECT status FROM kyc_checks WHERE id = $1`, checkID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// startedCheck is an Admin's account with SMS selected and an
// individual check started, returning the account, the check and its
// vendor reference.
func (h harness) startedCheck(t *testing.T) (tenant, checkID uuid.UUID, ref string) {
	t.Helper()
	ctx := context.Background()
	tenant, admin, _, _ := h.newTeam(t)
	if _, err := h.svc.SelectService(ctx, admin, tenant, "sms"); err != nil {
		t.Fatal(err)
	}
	check, err := h.svc.StartKYC(ctx, admin, tenant, domain.KYCKindIndividual)
	if err != nil {
		t.Fatalf("StartKYC: %v", err)
	}
	return tenant, check.ID, h.providerRef(t, check.ID)
}

func TestStartKYC(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, admin, _, viewer := h.newTeam(t)

	check, err := h.svc.StartKYC(ctx, admin, tenant, domain.KYCKindIndividual)
	if err != nil || check.Status != domain.KYCPending || check.URL == "" {
		t.Fatalf("got %+v, %v", check, err)
	}
	var subject uuid.UUID
	var provider string
	if err := h.super.QueryRow(ctx, `SELECT subject_id, provider FROM kyc_checks WHERE id = $1`, check.ID).Scan(&subject, &provider); err != nil {
		t.Fatal(err)
	}
	if subject != admin || provider != "fake" {
		t.Fatalf("subject %s, provider %s; want the Admin and fake", subject, provider)
	}
	data := assertEventValid(t, h, "kyc.check_started", tenant)
	if data["check_id"] != check.ID.String() || data["started_by"] != admin.String() {
		t.Fatalf("got event data %v", data)
	}

	if _, err := h.svc.StartKYC(ctx, viewer, tenant, domain.KYCKindIndividual); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Fatalf("a Viewer: got %v, want ErrPermissionDenied", err)
	}
	if _, err := h.svc.StartKYC(ctx, admin, tenant, domain.KYCKindBusiness); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("a business check before KYB: got %v, want ErrValidation", err)
	}
	checks, err := h.svc.ListKYCChecks(ctx, viewer, tenant)
	if err != nil || len(checks) != 1 || checks[0].ID != check.ID {
		t.Fatalf("a Viewer listing checks: %+v, %v", checks, err)
	}
}

func TestKYCWebhook_PassPromotesToStarter(t *testing.T) {
	h := newHarness(t)
	tenant, checkID, ref := h.startedCheck(t)

	if err := h.deliver(t, ref, domain.KYCPassed, kycvendor.NoTamper); err != nil {
		t.Fatalf("HandleKYCWebhook: %v", err)
	}
	if got := h.checkStatus(t, checkID); got != domain.KYCPassed {
		t.Fatalf("check %s, want passed", got)
	}
	if got := h.tierOf(t, tenant); got != "starter" {
		t.Fatalf("tier %s, want starter", got)
	}
	if n := h.count(t, `SELECT count(*) FROM tier_changes WHERE tenant_id = $1 AND kyc_check_id = $2 AND to_tier = 'starter'`, tenant, checkID); n != 1 {
		t.Fatalf("%d tier_changes rows for the check, want 1", n)
	}
	if data := assertEventValid(t, h, "tenant.tier_changed", tenant); data["kyc_check_id"] != checkID.String() {
		t.Fatalf("got event data %v", data)
	}
	if data := assertEventValid(t, h, "kyc.check_decided", tenant); data["status"] != "passed" || data["promoted"] != true {
		t.Fatalf("got event data %v", data)
	}
	if got := h.statusOf(t, tenant, "sms"); got != domain.ServiceEnabled {
		t.Fatalf("sms %s, want enabled by the promotion", got)
	}

	t.Run("the same result again changes nothing", func(t *testing.T) {
		if err := h.deliver(t, ref, domain.KYCPassed, kycvendor.NoTamper); err != nil {
			t.Fatal(err)
		}
		if n := h.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'kyc.check_decided' AND aggregate_id = $1`, tenant); n != 1 {
			t.Fatalf("%d decisions published, want 1", n)
		}
		if n := h.count(t, `SELECT count(*) FROM tier_changes WHERE tenant_id = $1`, tenant); n != 1 {
			t.Fatalf("%d tier changes, want 1", n)
		}
	})
	t.Run("a failure arriving after the pass never demotes", func(t *testing.T) {
		if err := h.deliver(t, ref, domain.KYCFailed, kycvendor.NoTamper); err != nil {
			t.Fatal(err)
		}
		if h.checkStatus(t, checkID) != domain.KYCPassed || h.tierOf(t, tenant) != "starter" {
			t.Fatalf("check %s, tier %s; want passed and starter", h.checkStatus(t, checkID), h.tierOf(t, tenant))
		}
	})
}

func TestKYCWebhook_ReviewThenPassed(t *testing.T) {
	h := newHarness(t)
	tenant, checkID, ref := h.startedCheck(t)
	if err := h.deliver(t, ref, domain.KYCReview, kycvendor.NoTamper); err != nil {
		t.Fatal(err)
	}
	if h.checkStatus(t, checkID) != domain.KYCReview || h.tierOf(t, tenant) != "sandbox" {
		t.Fatalf("after review: check %s, tier %s", h.checkStatus(t, checkID), h.tierOf(t, tenant))
	}
	if err := h.deliver(t, ref, domain.KYCPassed, kycvendor.NoTamper); err != nil {
		t.Fatal(err)
	}
	if h.checkStatus(t, checkID) != domain.KYCPassed || h.tierOf(t, tenant) != "starter" {
		t.Fatalf("after the decision: check %s, tier %s", h.checkStatus(t, checkID), h.tierOf(t, tenant))
	}
}

func TestKYCWebhook_FailedLeavesTheTier(t *testing.T) {
	h := newHarness(t)
	tenant, checkID, ref := h.startedCheck(t)
	if err := h.deliver(t, ref, domain.KYCFailed, kycvendor.NoTamper); err != nil {
		t.Fatal(err)
	}
	if h.checkStatus(t, checkID) != domain.KYCFailed || h.tierOf(t, tenant) != "sandbox" {
		t.Fatalf("check %s, tier %s", h.checkStatus(t, checkID), h.tierOf(t, tenant))
	}
	if data := assertEventValid(t, h, "kyc.check_decided", tenant); data["status"] != "failed" || data["promoted"] != false {
		t.Fatalf("got event data %v", data)
	}
	t.Run("a pass after the failure is ignored: start a new check", func(t *testing.T) {
		if err := h.deliver(t, ref, domain.KYCPassed, kycvendor.NoTamper); err != nil {
			t.Fatal(err)
		}
		if h.checkStatus(t, checkID) != domain.KYCFailed || h.tierOf(t, tenant) != "sandbox" {
			t.Fatalf("check %s, tier %s", h.checkStatus(t, checkID), h.tierOf(t, tenant))
		}
	})
}

func TestKYCWebhook_Refusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, checkID, ref := h.startedCheck(t)

	for name, tamper := range map[string]kycvendor.Tamper{
		"a tampered body": kycvendor.TamperBody, "another secret": kycvendor.WrongSecret,
		"a stale timestamp": kycvendor.Stale, "no signature": kycvendor.Unsigned,
	} {
		t.Run(name+" changes nothing", func(t *testing.T) {
			if err := h.deliver(t, ref, domain.KYCPassed, tamper); !errors.Is(err, app.ErrBadWebhook) {
				t.Fatalf("got %v, want ErrBadWebhook", err)
			}
			if h.checkStatus(t, checkID) != domain.KYCPending || h.tierOf(t, tenant) != "sandbox" {
				t.Fatal("a refused webhook changed the check or the tier")
			}
		})
	}
	t.Run("another provider's route is not found", func(t *testing.T) {
		header, body := h.vendor.Webhook(ref, domain.KYCPassed, kycvendor.NoTamper)
		if err := h.svc.HandleKYCWebhook(ctx, "acme-kyc", header, body); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("an unknown reference is acknowledged and ignored", func(t *testing.T) {
		if err := h.deliver(t, "fkyc_unknown", domain.KYCPassed, kycvendor.NoTamper); err != nil {
			t.Fatalf("got %v, want nil", err)
		}
	})
}

func TestKYCWebhook_SuspendedAccountIsNotPromoted(t *testing.T) {
	h := newHarness(t)
	tenant, checkID, ref := h.startedCheck(t)
	if _, err := h.super.Exec(context.Background(), `UPDATE tenants SET status = 'suspended' WHERE id = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := h.deliver(t, ref, domain.KYCPassed, kycvendor.NoTamper); err != nil {
		t.Fatal(err)
	}
	if h.checkStatus(t, checkID) != domain.KYCPassed || h.tierOf(t, tenant) != "sandbox" {
		t.Fatalf("check %s, tier %s; want passed and still sandbox", h.checkStatus(t, checkID), h.tierOf(t, tenant))
	}
}
