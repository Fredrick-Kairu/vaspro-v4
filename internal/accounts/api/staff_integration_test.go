//go:build integration

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
)

// staffOperations are the contract's staff operations (every path under
// /v1/accounts/staff/), so one added later is covered by the
// staff-only test the moment it's in the spec.
func staffOperations(t *testing.T) []struct{ Method, Path, ID string } {
	t.Helper()
	spec, err := openapi3.NewLoader().LoadFromFile("../../../docs/api/accounts.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var ops []struct{ Method, Path, ID string }
	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/v1/accounts/staff/") {
			continue
		}
		for method, op := range item.Operations() {
			ops = append(ops, struct{ Method, Path, ID string }{method, path, op.OperationID})
		}
	}
	if len(ops) < 6 {
		t.Fatalf("found %d staff operations in the spec, want at least 6", len(ops))
	}
	return ops
}

// kybInReview makes an account through the API whose KYB is in review,
// returning the account and the check.
func kybInReview(t *testing.T, r http.Handler, pool *vasdb.Pool, admin uuid.UUID) (tenant, check string) {
	t.Helper()
	ctx := context.Background()
	_, created := do(t, r, http.MethodPost, "/v1/accounts/tenants", admin, "create-"+admin.String(), map[string]string{"display_name": "Team", "country_code": "KE"})
	tenant, _ = created["id"].(string)
	profilePath := "/v1/accounts/tenants/" + tenant + "/business-profile"
	if status, got := do(t, r, http.MethodPut, profilePath, admin, "profile-"+admin.String(),
		map[string]string{"registration_number": "PVT-" + admin.String()[:8], "registered_name": "Acme"}); status != http.StatusOK {
		t.Fatalf("profile: %d %v", status, got)
	}
	if status, got := do(t, r, http.MethodPost, profilePath+"/owners", admin, "owner-"+admin.String(),
		map[string]string{"full_name": "Grace Wanjiru", "role": "director"}); status != http.StatusCreated {
		t.Fatalf("owner: %d %v", status, got)
	}
	for _, docType := range []string{"incorporation_cert", "tax_certificate", "proof_of_address"} {
		docID := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO documents (id, tenant_id, type, storage_key, sha256, mime_type, size_bytes, status, uploaded_by)
			VALUES ($1, $2, $3, 'fixture', $4, 'application/pdf', 1, 'received', $5)`, docID, tenant, docType, docID[:], admin); err != nil {
			t.Fatal(err)
		}
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE tenants SET tier = 'starter' WHERE id = $1`, tenant); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO tier_changes (id, tenant_id, from_tier, to_tier, reason, changed_by)
			VALUES ($1, $2, 'sandbox', 'starter', 'fixture', $3)`, uuid.New(), tenant, uuid.New())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	status, submitted := do(t, r, http.MethodPost, "/v1/accounts/tenants/"+tenant+"/kyb-submissions", admin, "submit-"+admin.String(), nil)
	if status != http.StatusCreated {
		t.Fatalf("submit: %d %v", status, submitted)
	}
	check, _ = submitted["id"].(string)
	return tenant, check
}

func TestStaffAPI_EveryStaffOperationIsStaffOnly(t *testing.T) {
	r, pool, _ := newRouterWithIdentity(t)
	admin := uuid.New()
	tenant, check := kybInReview(t, r, pool, admin)
	body := map[string]any{"decision": "passed", "reason": "x", "to": "financial"}

	for _, op := range staffOperations(t) {
		path := strings.NewReplacer("{id}", tenant, "{checkId}", check, "{docId}", uuid.NewString()).Replace(op.Path)
		var reqBody any
		if op.Method != http.MethodGet {
			reqBody = body
		}
		t.Run(op.ID, func(t *testing.T) {
			if status, got := do(t, r, op.Method, path, uuid.Nil, "anon-"+op.ID, reqBody); status != http.StatusUnauthorized {
				t.Fatalf("no user: got %d %v, want 401", status, got)
			}
			status, got := do(t, r, op.Method, path, admin, "admin-"+op.ID, reqBody)
			if status != http.StatusForbidden || got["code"] != "AUTH-4025" {
				t.Fatalf("the account's own Admin: got %d %v, want 403 AUTH-4025", status, got)
			}
		})
	}
	var status, tier, accountType string
	if err := pool.QueryRow(context.Background(), `SELECT k.status, t.tier, t.type FROM kyc_checks k JOIN tenants t ON t.id = k.tenant_id WHERE k.id = $1`, check).
		Scan(&status, &tier, &accountType); err != nil {
		t.Fatal(err)
	}
	if status != "review" || tier != "starter" || accountType != "direct" {
		t.Fatalf("after refusals: check %s, tier %s, type %s; want nothing changed", status, tier, accountType)
	}
}

func TestStaffAPI(t *testing.T) {
	r, pool, identity := newRouterWithIdentity(t)
	staff := uuid.New()
	identity.SetStaff(staff, true)
	tenant, check := kybInReview(t, r, pool, uuid.New())
	reviewPath := "/v1/accounts/staff/kyc-reviews/" + check
	staffTenant := "/v1/accounts/staff/tenants/" + tenant

	status, queue := do(t, r, http.MethodGet, "/v1/accounts/staff/kyc-reviews?limit=10", staff, "", nil)
	reviews, _ := queue["reviews"].([]any)
	if status != http.StatusOK || len(reviews) != 1 || reviews[0].(map[string]any)["check_id"] != check ||
		reviews[0].(map[string]any)["duplicate_registration"] != false {
		t.Fatalf("the queue: got %d %v", status, queue)
	}
	for _, q := range []string{"?limit=500", "?cursor=not-a-cursor"} {
		if status, got := do(t, r, http.MethodGet, "/v1/accounts/staff/kyc-reviews"+q, staff, "", nil); status != http.StatusBadRequest || got["code"] != "VAL-4000" {
			t.Fatalf("%s: got %d %v, want 400 VAL-4000", q, status, got)
		}
	}

	status, review := do(t, r, http.MethodGet, reviewPath, staff, "", nil)
	docs, _ := review["documents"].([]any)
	if status != http.StatusOK || review["business_profile"] == nil || len(docs) != 3 {
		t.Fatalf("the review: got %d %v", status, review)
	}
	if status, got := do(t, r, http.MethodGet, "/v1/accounts/staff/kyc-reviews/"+uuid.NewString(), staff, "", nil); status != http.StatusNotFound || got["code"] != "VAL-4003" {
		t.Fatalf("an unknown check: got %d %v", status, got)
	}

	if status, got := do(t, r, http.MethodPost, reviewPath+"/decision", staff, "decide-bad",
		map[string]any{"decision": "passed", "reason": " "}); status != http.StatusBadRequest || got["code"] != "VAL-4000" {
		t.Fatalf("no reason: got %d %v", status, got)
	}
	status, decided := do(t, r, http.MethodPost, reviewPath+"/decision", staff, "decide",
		map[string]any{"decision": "passed", "reason": "matches the registry"})
	if status != http.StatusOK || decided["status"] != "passed" || decided["reason"] != "matches the registry" {
		t.Fatalf("deciding: got %d %v", status, decided)
	}
	if status, got := do(t, r, http.MethodPost, reviewPath+"/decision", staff, "decide-again",
		map[string]any{"decision": "failed", "reason": "no"}); status != http.StatusConflict || got["code"] != "VAL-4010" {
		t.Fatalf("deciding again: got %d %v", status, got)
	}
	if status, got := do(t, r, http.MethodGet, reviewPath, staff, "", nil); status != http.StatusConflict || got["code"] != "VAL-4010" {
		t.Fatalf("reading a decided check: got %d %v", status, got)
	}

	if status, got := do(t, r, http.MethodPost, staffTenant+"/reseller", staff, "reseller-early",
		map[string]string{"reason": "agreement"}); status != http.StatusForbidden || got["code"] != "KYC-4020" {
		t.Fatalf("a reseller on business: got %d %v", status, got)
	}
	if status, got := do(t, r, http.MethodPost, staffTenant+"/tier-changes", staff, "tier-same",
		map[string]string{"to": "business", "reason": "again"}); status != http.StatusConflict || got["code"] != "VAL-4011" {
		t.Fatalf("the same tier: got %d %v", status, got)
	}
	if status, got := do(t, r, http.MethodPost, staffTenant+"/tier-changes", staff, "tier-bad",
		map[string]string{"to": "gold", "reason": "x"}); status != http.StatusBadRequest || got["code"] != "VAL-4000" {
		t.Fatalf("an unknown tier: got %d %v", status, got)
	}
	status, account := do(t, r, http.MethodPost, staffTenant+"/tier-changes", staff, "tier",
		map[string]string{"to": "financial", "reason": "documents on file"})
	if status != http.StatusOK || account["tier"] != "financial" {
		t.Fatalf("to financial: got %d %v", status, account)
	}
	status, account = do(t, r, http.MethodPost, staffTenant+"/reseller", staff, "reseller",
		map[string]string{"reason": "agreement"})
	if status != http.StatusOK || account["type"] != "reseller" {
		t.Fatalf("making a reseller: got %d %v", status, account)
	}
	if status, got := do(t, r, http.MethodPost, staffTenant+"/reseller", staff, "reseller-again",
		map[string]string{"reason": "agreement"}); status != http.StatusForbidden || got["code"] != "KYC-4021" {
		t.Fatalf("a reseller again: got %d %v", status, got)
	}
	if status, got := do(t, r, http.MethodPost, "/v1/accounts/staff/tenants/"+uuid.NewString()+"/tier-changes", staff, "tier-unknown",
		map[string]string{"to": "financial", "reason": "x"}); status != http.StatusNotFound || got["code"] != "VAL-4003" {
		t.Fatalf("an unknown account: got %d %v", status, got)
	}
}
