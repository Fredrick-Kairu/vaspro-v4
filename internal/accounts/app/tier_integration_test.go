//go:build integration

package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/identityusers"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/kycvendor"
)

var kycSecret = []byte("kyc-webhook-test-secret")

// harness runs app.Service as a non-superuser role, so RLS applies the
// way it will in production; super arranges fixtures and reads back.
type harness struct {
	svc      *app.Service
	app      *vasdb.Pool
	super    *vasdb.Pool
	identity *identityusers.Fake
	vendor   *kycvendor.Vendor
	objects  *objectstore.Store // only from newHarnessWithDocuments
	scanner  *clamd.Server      // only from newHarnessWithDocuments
}

func newHarness(t *testing.T) harness {
	t.Helper()
	return newHarnessWith(t, false)
}

// newHarnessWithDocuments is newHarness with real object storage
// (SeaweedFS) and the fake clamd behind documents (task 2.8); only the
// tests that need them pay for the container.
func newHarnessWithDocuments(t *testing.T) harness {
	t.Helper()
	return newHarnessWith(t, true)
}

func newHarnessWith(t *testing.T, documents bool) harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/accounts"))
	const setup = `
		CREATE ROLE accounts_role LOGIN PASSWORD 'accounts_role';
		GRANT USAGE ON SCHEMA accounts TO accounts_role;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA accounts TO accounts_role;
		GRANT accounts_membership_lookup TO accounts_role WITH INHERIT FALSE;
		GRANT accounts_kyc_webhook TO accounts_role WITH INHERIT FALSE;
		GRANT accounts_registration_lookup TO accounts_role WITH INHERIT FALSE;
		GRANT accounts_staff_review TO accounts_role WITH INHERIT FALSE;
	`
	if _, err := pg.Pool.Exec(ctx, setup); err != nil {
		t.Fatalf("setting up accounts_role: %v", err)
	}
	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=accounts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	appPool, err := vasdb.NewPool(ctx, pg.RoleDSN("accounts_role", "accounts_role")+"&search_path=accounts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(appPool.Close)
	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	identity := &identityusers.Fake{}
	vendor := kycvendor.New(kycSecret)
	t.Cleanup(vendor.Close)
	deps := app.Deps{
		Pool: appPool, Clock: testkit.NewClock(time.Now().UTC()), Packs: map[string]*packs.Pack{"KE": ke},
		Identity: identity, KYC: adapters.NewFakeVendorProvider(vendor.URL(), kycSecret),
	}
	var (
		objects *objectstore.Store
		fake    *clamd.Server
	)
	if documents {
		cfg := testkit.StartObjectStorage(t)
		cfg.Bucket = "documents"
		var err error
		if objects, err = objectstore.New(cfg); err != nil {
			t.Fatal(err)
		}
		if err := objects.EnsureBucket(ctx); err != nil {
			t.Fatal(err)
		}
		fake, err = clamd.Start()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(fake.Close)
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		kms, err := vascrypto.NewLocalKMS(map[string][]byte{"documents": key})
		if err != nil {
			t.Fatal(err)
		}
		deps.Documents = &app.DocumentDeps{
			Store: objects, Scanner: adapters.NewClamdScanner(fake.Addr()),
			Envelope: vascrypto.NewEnvelope(kms), KeyID: "documents",
		}
	}
	svc := app.New(deps)
	return harness{svc: svc, app: appPool, super: super, identity: identity, vendor: vendor, objects: objects, scanner: fake}
}

func (h harness) newTenant(t *testing.T, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO tenants (id, display_name, country_code, timezone, default_currency, region, status)
		VALUES ($1, 'Acme', 'KE', 'Africa/Nairobi', 'KES', 'ke', $2)`, id, status); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return id
}

func (h harness) newCheck(t *testing.T, tenant uuid.UUID, kind, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := store.New(h.super).CreateKycCheck(context.Background(), store.CreateKycCheckParams{
		ID: id, TenantID: tenant, Kind: kind, SubjectID: uuid.New(), Provider: "fake", Status: status,
	}); err != nil {
		t.Fatalf("insert kyc check: %v", err)
	}
	return id
}

func (h harness) tierOf(t *testing.T, tenant uuid.UUID) string {
	t.Helper()
	var tier string
	if err := h.super.QueryRow(context.Background(), `SELECT tier FROM tenants WHERE id = $1`, tenant).Scan(&tier); err != nil {
		t.Fatal(err)
	}
	return tier
}

func (h harness) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ptr[T any](v T) *T { return &v }

func TestChangeTier_PromotionWritesTierRowAndEventTogether(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant := h.newTenant(t, domain.TenantActive)
	check := h.newCheck(t, tenant, domain.KYCKindIndividual, "passed")

	err := h.svc.ChangeTier(ctx, app.ChangeTierRequest{
		TenantID: tenant, To: domain.TierStarter, Reason: "individual KYC passed", KYCCheckID: &check,
	})
	if err != nil {
		t.Fatalf("ChangeTier: %v", err)
	}
	if got := h.tierOf(t, tenant); got != "starter" {
		t.Fatalf("tier %q, want starter", got)
	}
	var row store.AccountsTierChange
	err = h.super.QueryRow(ctx, `SELECT id, tenant_id, from_tier, to_tier, reason, kyc_check_id, changed_by, created_at FROM tier_changes WHERE tenant_id = $1`, tenant).
		Scan(&row.ID, &row.TenantID, &row.FromTier, &row.ToTier, &row.Reason, &row.KycCheckID, &row.ChangedBy, &row.CreatedAt)
	if err != nil {
		t.Fatalf("reading tier_changes: %v", err)
	}
	if row.FromTier != "sandbox" || row.ToTier != "starter" || row.KycCheckID != (pgtype.UUID{Bytes: check, Valid: true}) || row.ChangedBy.Valid {
		t.Fatalf("got tier change %+v", row)
	}
	data := assertTierChangedEventValid(t, h, tenant)
	if data["from_tier"] != "sandbox" || data["to_tier"] != "starter" || data["kyc_check_id"] != check.String() {
		t.Fatalf("got event data %v", data)
	}
}

func TestChangeTier_StaffPromotesStraightToFinancial(t *testing.T) {
	h := newHarness(t)
	tenant := h.newTenant(t, domain.TenantActive)
	staffID := uuid.New()
	if err := h.svc.ChangeTier(context.Background(), app.ChangeTierRequest{
		TenantID: tenant, To: domain.TierFinancial, Reason: "existing client, documents on file", ChangedBy: &staffID,
	}); err != nil {
		t.Fatalf("ChangeTier: %v", err)
	}
	if got := h.tierOf(t, tenant); got != "financial" {
		t.Fatalf("tier %q, want financial", got)
	}
	data := assertTierChangedEventValid(t, h, tenant)
	if data["changed_by"] != staffID.String() {
		t.Fatalf("got event data %v, want changed_by %s", data, staffID)
	}
}

func TestChangeTier_RefusedChangesWriteNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	staffID := uuid.New()

	other := h.newTenant(t, domain.TenantActive)
	otherCheck := h.newCheck(t, other, domain.KYCKindIndividual, "passed")

	cases := []struct {
		name   string
		status string
		req    func(tenant uuid.UUID) app.ChangeTierRequest
		want   error
	}{
		{"a check skipping tiers", domain.TenantActive, func(tenant uuid.UUID) app.ChangeTierRequest {
			c := h.newCheck(t, tenant, domain.KYCKindBusiness, "passed")
			return app.ChangeTierRequest{TenantID: tenant, To: domain.TierBusiness, Reason: "kyb", KYCCheckID: &c}
		}, domain.ErrSkipsTiers},
		{"a check that hasn't passed", domain.TenantActive, func(tenant uuid.UUID) app.ChangeTierRequest {
			c := h.newCheck(t, tenant, domain.KYCKindIndividual, "review")
			return app.ChangeTierRequest{TenantID: tenant, To: domain.TierStarter, Reason: "kyc", KYCCheckID: &c}
		}, app.ErrCheckNotUsable},
		{"another tenant's check", domain.TenantActive, func(tenant uuid.UUID) app.ChangeTierRequest {
			return app.ChangeTierRequest{TenantID: tenant, To: domain.TierStarter, Reason: "kyc", KYCCheckID: &otherCheck}
		}, app.ErrCheckNotUsable},
		{"a check that doesn't exist", domain.TenantActive, func(tenant uuid.UUID) app.ChangeTierRequest {
			return app.ChangeTierRequest{TenantID: tenant, To: domain.TierStarter, Reason: "kyc", KYCCheckID: ptr(uuid.New())}
		}, app.ErrCheckNotUsable},
		{"no evidence", domain.TenantActive, func(tenant uuid.UUID) app.ChangeTierRequest {
			return app.ChangeTierRequest{TenantID: tenant, To: domain.TierStarter, Reason: "because"}
		}, domain.ErrNoEvidence},
		{"promoting a suspended tenant", domain.TenantSuspended, func(tenant uuid.UUID) app.ChangeTierRequest {
			return app.ChangeTierRequest{TenantID: tenant, To: domain.TierStarter, Reason: "reviewed", ChangedBy: &staffID}
		}, domain.ErrPromotionWhileInactive},
		{"a tenant that doesn't exist", domain.TenantActive, func(uuid.UUID) app.ChangeTierRequest {
			return app.ChangeTierRequest{TenantID: uuid.New(), To: domain.TierStarter, Reason: "reviewed", ChangedBy: &staffID}
		}, app.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant := h.newTenant(t, tc.status)
			err := h.svc.ChangeTier(ctx, tc.req(tenant))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if got := h.tierOf(t, tenant); got != "sandbox" {
				t.Fatalf("tier changed to %q", got)
			}
			if n := h.count(t, `SELECT count(*) FROM tier_changes WHERE tenant_id = $1`, tenant); n != 0 {
				t.Fatalf("%d tier_changes rows written", n)
			}
			if n := h.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, tenant); n != 0 {
				t.Fatalf("%d events written", n)
			}
		})
	}
}

func TestChangeTier_DemotionByReverification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant := h.newTenant(t, domain.TenantActive)
	staffID := uuid.New()
	if err := h.svc.ChangeTier(ctx, app.ChangeTierRequest{TenantID: tenant, To: domain.TierBusiness, Reason: "reviewed", ChangedBy: &staffID}); err != nil {
		t.Fatal(err)
	}
	failed := h.newCheck(t, tenant, domain.KYCKindBusiness, "failed")
	if err := h.svc.ChangeTier(ctx, app.ChangeTierRequest{TenantID: tenant, To: domain.TierStarter, Reason: "directors changed; KYB failed", KYCCheckID: &failed}); err != nil {
		t.Fatalf("demotion: %v", err)
	}
	if got := h.tierOf(t, tenant); got != "starter" {
		t.Fatalf("tier %q, want starter", got)
	}
	if n := h.count(t, `SELECT count(*) FROM tier_changes WHERE tenant_id = $1`, tenant); n != 2 {
		t.Fatalf("%d tier_changes rows, want 2", n)
	}
}

// assertTierChangedEventValid checks the newest tenant.tier_changed
// event for tenant against docs/events/tenant.tier_changed.schema.json
// (ROADMAP §8.3) and returns its data.
func assertTierChangedEventValid(t *testing.T, h harness, tenant uuid.UUID) map[string]any {
	t.Helper()
	return assertEventValid(t, h, "tenant.tier_changed", tenant)
}

// assertEventValid checks the newest eventType event for tenant against
// docs/events/<eventType>.schema.json (ROADMAP §8.3) and returns its
// data.
func assertEventValid(t *testing.T, h harness, eventType string, tenant uuid.UUID) map[string]any {
	t.Helper()
	return assertEventValidWhere(t, h, eventType, tenant, "", "")
}

// assertEventValidWhere is assertEventValid for the newest event whose
// data has field = value (when field isn't empty). The harness clock
// doesn't move, so several events of one type in a test share a
// timestamp and "newest" alone can't tell them apart.
func assertEventValidWhere(t *testing.T, h harness, eventType string, tenant uuid.UUID, field, value string) map[string]any {
	t.Helper()
	var payload []byte
	err := h.super.QueryRow(context.Background(),
		`SELECT payload FROM outbox_events WHERE event_type = $2 AND aggregate_id = $1
		 AND ($3 = '' OR payload->'data'->>$3 = $4)
		 ORDER BY occurred_at DESC LIMIT 1`, tenant, eventType, field, value).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no %s event in the outbox", eventType)
	}
	if err != nil {
		t.Fatal(err)
	}
	var env platformevents.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.TenantID != tenant {
		t.Fatalf("envelope tenant %s, want %s", env.TenantID, tenant)
	}
	path := "../../../docs/events/" + eventType + ".schema.json"
	raw, err := os.ReadFile(path) //nolint:gosec // eventType is a fixed name from the test itself
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(eventType+".schema.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(eventType + ".schema.json")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(env.Data))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("event data %s fails its schema: %v", env.Data, err)
	}
	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}
