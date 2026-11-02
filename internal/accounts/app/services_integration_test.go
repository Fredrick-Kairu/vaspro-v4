//go:build integration

package app_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
)

// kePack is the real KE pack with its service rules replaced.
func kePack(t *testing.T, rules map[string]packs.ServiceRule) map[string]*packs.Pack {
	t.Helper()
	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if rules != nil {
		ke.Services = rules
	}
	return map[string]*packs.Pack{"KE": ke}
}

func serviceByCode(t *testing.T, cat *app.ServiceCatalogue, code string) app.AccountService {
	t.Helper()
	for _, s := range cat.Services {
		if s.Code == code {
			return s
		}
	}
	t.Fatalf("%s isn't in the catalogue: %+v", code, cat.Services)
	return app.AccountService{}
}

func (h harness) statusOf(t *testing.T, tenant uuid.UUID, code string) string {
	t.Helper()
	var status string
	if err := h.super.QueryRow(context.Background(),
		`SELECT status FROM tenant_services WHERE tenant_id = $1 AND service_code = $2`, tenant, code).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestSyncServiceRules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rules := func() map[string]string {
		got := map[string]string{}
		rows, err := h.super.Query(ctx, `SELECT service_code, min_tier::text || ':' || enabled::text FROM service_country_rules WHERE country_code = 'KE'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var code, v string
			if err := rows.Scan(&code, &v); err != nil {
				t.Fatal(err)
			}
			got[code] = v
		}
		return got
	}

	if err := h.svc.SyncServiceRules(ctx, kePack(t, nil)); err != nil {
		t.Fatalf("SyncServiceRules: %v", err)
	}
	if got := rules(); len(got) != 7 || got["sms"] != "starter:true" || got["payouts"] != "financial:true" {
		t.Fatalf("after syncing the real pack: %v", got)
	}
	if err := h.svc.SyncServiceRules(ctx, kePack(t, nil)); err != nil {
		t.Fatalf("syncing twice: %v", err)
	}

	changed := map[string]packs.ServiceRule{"sms": {MinTier: "business", Enabled: true}, "ussd": {MinTier: "starter", Enabled: false}}
	if err := h.svc.SyncServiceRules(ctx, kePack(t, changed)); err != nil {
		t.Fatal(err)
	}
	if got := rules(); len(got) != 2 || got["sms"] != "business:true" || got["ussd"] != "starter:false" {
		t.Fatalf("after the pack changed: %v, want only sms and ussd (the rest removed)", got)
	}

	unknown := map[string]packs.ServiceRule{"smsx": {MinTier: "starter", Enabled: true}}
	if err := h.svc.SyncServiceRules(ctx, kePack(t, unknown)); !errors.Is(err, app.ErrUnknownService) {
		t.Fatalf("a code outside the catalogue: got %v, want ErrUnknownService", err)
	}
}

func TestServiceSelectionAndTiers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rules := maps.Clone(kePack(t, nil)["KE"].Services)
	rules["rewards"] = packs.ServiceRule{MinTier: "business", Enabled: false}
	if err := h.svc.SyncServiceRules(ctx, kePack(t, rules)); err != nil {
		t.Fatal(err)
	}
	tenant, admin, _, viewer := h.newTeam(t)

	for _, code := range []string{"sms", "whatsapp"} {
		s, err := h.svc.SelectService(ctx, admin, tenant, code)
		if err != nil || !s.Selected || s.Status != domain.ServiceSelected || !s.Locked {
			t.Fatalf("selecting %s in the sandbox: got %+v, %v; want selected and locked", code, s, err)
		}
	}
	data := assertEventValidWhere(t, h, "service.selected", tenant, "service_code", "whatsapp")
	if data["service_code"] != "whatsapp" || data["status"] != "selected" || data["selected_by"] != admin.String() {
		t.Fatalf("got event data %v", data)
	}

	cat, err := h.svc.ListServices(ctx, viewer, tenant)
	if err != nil {
		t.Fatalf("a Viewer listing services: %v", err)
	}
	if cat.Tier != domain.TierSandbox || cat.RequiredTier != domain.TierBusiness {
		t.Fatalf("tier %s, required %s; want sandbox and business", cat.Tier, cat.RequiredTier)
	}
	if s := serviceByCode(t, cat, "sms"); !s.Locked || s.MinTier != domain.TierStarter {
		t.Fatalf("sms: %+v, want locked, needing starter", s)
	}
	if s := serviceByCode(t, cat, "whatsapp"); !s.Locked || s.MinTier != domain.TierBusiness {
		t.Fatalf("whatsapp: %+v, want locked, needing business", s)
	}
	if s := serviceByCode(t, cat, "ussd"); s.Selected || s.Locked {
		t.Fatalf("ussd, not selected: %+v", s)
	}
	for _, s := range cat.Services {
		if s.Code == "rewards" {
			t.Fatal("a service the pack turns off is listed")
		}
	}

	t.Run("refusals", func(t *testing.T) {
		if _, err := h.svc.SelectService(ctx, admin, tenant, "rewards"); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("a service off in KE: got %v, want ErrNotFound", err)
		}
		if _, err := h.svc.SelectService(ctx, admin, tenant, "voice"); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("an unknown service: got %v, want ErrNotFound", err)
		}
		if _, err := h.svc.SelectService(ctx, viewer, tenant, "ussd"); !errors.Is(err, domain.ErrPermissionDenied) {
			t.Fatalf("a Viewer selecting: got %v, want ErrPermissionDenied", err)
		}
		if err := h.svc.DeselectService(ctx, viewer, tenant, "sms"); !errors.Is(err, domain.ErrPermissionDenied) {
			t.Fatalf("a Viewer deselecting: got %v, want ErrPermissionDenied", err)
		}
	})
	t.Run("selecting again changes nothing", func(t *testing.T) {
		before := h.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'service.selected'`)
		if s, err := h.svc.SelectService(ctx, admin, tenant, "sms"); err != nil || s.Status != domain.ServiceSelected {
			t.Fatalf("got %+v, %v", s, err)
		}
		if after := h.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'service.selected'`); after != before {
			t.Fatal("selecting again wrote another event")
		}
	})

	t.Run("reaching starter enables SMS but not WhatsApp", func(t *testing.T) {
		if err := h.svc.ChangeTier(ctx, app.ChangeTierRequest{TenantID: tenant, To: domain.TierStarter, Reason: "kyc", ChangedBy: ptr(uuid.New())}); err != nil {
			t.Fatal(err)
		}
		if h.statusOf(t, tenant, "sms") != domain.ServiceEnabled || h.statusOf(t, tenant, "whatsapp") != domain.ServiceSelected {
			t.Fatalf("sms %s, whatsapp %s", h.statusOf(t, tenant, "sms"), h.statusOf(t, tenant, "whatsapp"))
		}
		if data := assertEventValidWhere(t, h, "service.enabled", tenant, "service_code", "sms"); data["service_code"] != "sms" {
			t.Fatalf("got event data %v", data)
		}
	})
	t.Run("a service selected at or below the tier is enabled at once", func(t *testing.T) {
		s, err := h.svc.SelectService(ctx, admin, tenant, "ussd")
		if err != nil || s.Status != domain.ServiceEnabled || s.Locked {
			t.Fatalf("got %+v, %v", s, err)
		}
	})
	t.Run("a suspended service is left alone by tier changes", func(t *testing.T) {
		if _, err := h.super.Exec(ctx, `UPDATE tenant_services SET status = 'suspended' WHERE tenant_id = $1 AND service_code = 'ussd'`, tenant); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a demotion locks SMS again", func(t *testing.T) {
		if err := h.svc.ChangeTier(ctx, app.ChangeTierRequest{TenantID: tenant, To: domain.TierSandbox, Reason: "fraud", ChangedBy: ptr(uuid.New())}); err != nil {
			t.Fatal(err)
		}
		if got := h.statusOf(t, tenant, "sms"); got != domain.ServiceSelected {
			t.Fatalf("sms %s, want selected", got)
		}
		if got := h.statusOf(t, tenant, "ussd"); got != domain.ServiceSuspended {
			t.Fatalf("ussd %s, want still suspended", got)
		}
		if data := assertEventValidWhere(t, h, "service.locked", tenant, "service_code", "sms"); data["service_code"] != "sms" || data["min_tier"] != "starter" {
			t.Fatalf("got event data %v", data)
		}
		if n := h.count(t, `SELECT count(*) FROM tenant_services WHERE tenant_id = $1 AND service_code = 'sms' AND enabled_at IS NOT NULL`, tenant); n != 0 {
			t.Fatal("a locked service keeps its enabled_at")
		}
	})
	t.Run("deselecting drops the required tier", func(t *testing.T) {
		if err := h.svc.DeselectService(ctx, admin, tenant, "whatsapp"); err != nil {
			t.Fatal(err)
		}
		if data := assertEventValid(t, h, "service.deselected", tenant); data["service_code"] != "whatsapp" {
			t.Fatalf("got event data %v", data)
		}
		cat, err := h.svc.ListServices(ctx, admin, tenant)
		if err != nil || cat.RequiredTier != domain.TierStarter {
			t.Fatalf("got %+v, %v; want starter required", cat, err)
		}
		if err := h.svc.DeselectService(ctx, admin, tenant, "whatsapp"); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("deselecting again: got %v, want ErrNotFound", err)
		}
	})
}
