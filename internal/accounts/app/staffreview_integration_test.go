//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/kycvendor"
)

// newStaff is a user identity says is VASPRO staff (task 2.9).
func (h harness) newStaff() uuid.UUID {
	id := uuid.New()
	h.identity.SetStaff(id, true)
	return id
}

// businessReview is a starter account whose KYB is in review: a
// profile with a director, the pack's documents, then submitted. regNo
// keeps tests from flagging each other as duplicates.
func (h harness) businessReview(t *testing.T, regNo string) (tenant, admin, check uuid.UUID, docs []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant, admin, _ = h.starterTeam(t)
	profile := acmeProfile
	profile.RegistrationNumber = regNo
	if _, err := h.svc.SetBusinessProfile(ctx, admin, tenant, profile); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AddOwner(ctx, admin, tenant, domain.OwnerInput{FullName: "Grace Wanjiru", Role: domain.OwnerDirector}); err != nil {
		t.Fatal(err)
	}
	docs = h.addKYBDocuments(t, tenant)
	submitted, err := h.svc.SubmitKYB(ctx, admin, tenant)
	if err != nil {
		t.Fatalf("SubmitKYB: %v", err)
	}
	return tenant, admin, submitted.ID, docs
}

func (h harness) documentStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := h.super.QueryRow(context.Background(), `SELECT status FROM documents WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestStaffActions_StaffOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, admin, check, docs := h.businessReview(t, "PVT-STAFFONLY")
	revoked := h.newStaff()
	h.identity.SetStaff(revoked, false)

	calls := map[string]func(caller uuid.UUID) error{
		"list the queue": func(c uuid.UUID) error { _, err := h.svc.ListKYCReviews(ctx, c, "", 10); return err },
		"read a review":  func(c uuid.UUID) error { _, err := h.svc.GetKYCReview(ctx, c, check); return err },
		"decide": func(c uuid.UUID) error {
			_, err := h.svc.DecideKYCReview(ctx, c, check, domain.StaffDecision{Decision: domain.KYCPassed, Reason: "ok"})
			return err
		},
		"change the tier": func(c uuid.UUID) error {
			_, err := h.svc.StaffChangeTier(ctx, c, tenant, domain.TierFinancial, "ok", nil)
			return err
		},
		"make a reseller": func(c uuid.UUID) error { _, err := h.svc.StaffMakeReseller(ctx, c, tenant, "ok"); return err },
		"download a document": func(c uuid.UUID) error {
			_, _, err := h.svc.StaffDownloadDocument(ctx, c, tenant, docs[0])
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			for who, caller := range map[string]uuid.UUID{"the account's own Admin": admin, "a stranger": uuid.New(), "revoked staff": revoked} {
				if err := call(caller); !errors.Is(err, app.ErrNotStaff) {
					t.Fatalf("%s: got %v, want ErrNotStaff", who, err)
				}
			}
		})
	}
	t.Run("identity unreachable fails closed", func(t *testing.T) {
		staff := h.newStaff()
		h.identity.Unreachable = true
		defer func() { h.identity.Unreachable = false }()
		if err := calls["decide"](staff); err == nil || errors.Is(err, app.ErrNotStaff) {
			t.Fatalf("got %v, want an error that isn't a plain refusal", err)
		}
	})
	if got := h.checkStatus(t, check); got != domain.KYCReview {
		t.Fatalf("the check is %s after refusals, want review", got)
	}
	if got := h.tierOf(t, tenant); got != string(domain.TierStarter) {
		t.Fatalf("tier %s after refusals, want starter", got)
	}
	if got := h.typeOf(t, tenant); got != "direct" {
		t.Fatalf("type %s after refusals, want direct", got)
	}
}

func TestKYCReviewQueue(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	staff := h.newStaff()

	// Another account with the first one's registration number, so the
	// first is flagged as a duplicate.
	elsewhere, elsewhereAdmin, _ := h.starterTeam(t)
	dup := acmeProfile
	dup.RegistrationNumber = "PVT-QUEUE0"
	if _, err := h.svc.SetBusinessProfile(ctx, elsewhereAdmin, elsewhere, dup); err != nil {
		t.Fatal(err)
	}
	var checks []uuid.UUID
	for i := 0; i < 3; i++ {
		_, _, check, _ := h.businessReview(t, fmt.Sprintf("PVT-QUEUE%d", i))
		checks = append(checks, check)
	}
	// A vendor's individual check in review joins the queue too.
	_, individual, ref := h.startedCheck(t)
	if err := h.deliver(t, ref, domain.KYCReview, kycvendor.NoTamper); err != nil {
		t.Fatal(err)
	}
	checks = append(checks, individual)

	var seen []uuid.UUID
	cursor := ""
	for pages := 0; ; pages++ {
		page, err := h.svc.ListKYCReviews(ctx, staff, cursor, 3)
		if err != nil {
			t.Fatalf("ListKYCReviews: %v", err)
		}
		for _, r := range page.Reviews {
			seen = append(seen, r.CheckID)
		}
		if pages == 0 {
			first := page.Reviews[0]
			if first.CheckID != checks[0] || !first.DuplicateRegistration || first.OtherAccounts != 1 ||
				first.AccountName != "Team" || first.CountryCode != "KE" || first.Tier != string(domain.TierStarter) || first.Kind != domain.KYCKindBusiness {
				t.Fatalf("first review %+v; want the oldest check, flagged, with its account", first)
			}
			if page.Reviews[1].DuplicateRegistration {
				t.Fatal("a unique registration number is flagged")
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 3 {
			t.Fatal("the cursor never ends")
		}
	}
	if fmt.Sprint(seen) != fmt.Sprint(checks) {
		t.Fatalf("queue order %v, want oldest first %v", seen, checks)
	}

	if _, err := h.svc.DecideKYCReview(ctx, staff, checks[1], domain.StaffDecision{Decision: domain.KYCFailed, Reason: "blurred"}); err != nil {
		t.Fatal(err)
	}
	page, err := h.svc.ListKYCReviews(ctx, staff, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Reviews {
		if r.CheckID == checks[1] {
			t.Fatal("a decided check is still in the queue")
		}
	}
	if _, err := h.svc.ListKYCReviews(ctx, staff, "not-a-cursor", 10); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("a malformed cursor: got %v, want ErrValidation", err)
	}
}

func TestGetKYCReview(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	staff := h.newStaff()

	tenant, _, check, docs := h.businessReview(t, "PVT-DETAIL")
	review, err := h.svc.GetKYCReview(ctx, staff, check)
	if err != nil {
		t.Fatalf("GetKYCReview: %v", err)
	}
	if review.Summary.TenantID != tenant || review.Check.Status != domain.KYCReview || review.BusinessProfile == nil ||
		review.BusinessProfile.RegistrationNumber != "PVT-DETAIL" || len(review.BusinessProfile.Owners) != 1 || len(review.Documents) != len(docs) {
		t.Fatalf("business review %+v; want the profile, its director and the three documents", review)
	}

	// The subject must be a user identity knows, for their email.
	subject := h.identity.AddUser("subject@example.com", "Subject Person")
	created, err := h.svc.CreateTenant(ctx, subject, domain.NewTenantInput{DisplayName: "Solo", CountryCode: "KE"})
	if err != nil {
		t.Fatal(err)
	}
	started, err := h.svc.StartKYC(ctx, subject, created.ID, domain.KYCKindIndividual)
	if err != nil {
		t.Fatal(err)
	}
	individual := started.ID
	if err := h.deliver(t, h.providerRef(t, individual), domain.KYCReview, kycvendor.NoTamper); err != nil {
		t.Fatal(err)
	}
	review, err = h.svc.GetKYCReview(ctx, staff, individual)
	if err != nil {
		t.Fatal(err)
	}
	if review.SubjectEmail != "subject@example.com" || review.SubjectName != "Subject Person" || review.BusinessProfile != nil {
		t.Fatalf("individual review %+v; want the subject's email and no business profile", review)
	}

	if _, err := h.svc.GetKYCReview(ctx, staff, uuid.New()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("an unknown check: got %v, want ErrNotFound", err)
	}
	if _, err := h.svc.DecideKYCReview(ctx, staff, check, domain.StaffDecision{Decision: domain.KYCPassed, Reason: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.GetKYCReview(ctx, staff, check); !errors.Is(err, app.ErrAlreadyDecided) {
		t.Fatalf("a decided check: got %v, want ErrAlreadyDecided", err)
	}
}

func TestDecideKYCReview_BusinessPass(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	staff := h.newStaff()
	tenant, admin, check, docs := h.businessReview(t, "PVT-PASS")

	decided, err := h.svc.DecideKYCReview(ctx, staff, check, domain.StaffDecision{Decision: domain.KYCPassed, Reason: "matches the registry"})
	if err != nil {
		t.Fatalf("DecideKYCReview: %v", err)
	}
	if decided.Status != domain.KYCPassed || decided.Reason != "matches the registry" || decided.DecidedAt == nil {
		t.Fatalf("decided %+v", decided)
	}
	if got := h.tierOf(t, tenant); got != string(domain.TierBusiness) {
		t.Fatalf("tier %s, want business", got)
	}
	if n := h.count(t, `SELECT count(*) FROM tier_changes WHERE tenant_id = $1 AND to_tier = 'business' AND kyc_check_id = $2 AND changed_by = $3`, tenant, check, staff); n != 1 {
		t.Fatalf("%d tier_changes rows with the check and the reviewer, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM kyc_checks WHERE id = $1 AND reviewed_by = $2 AND decided_at IS NOT NULL`, check, staff); n != 1 {
		t.Fatal("the check doesn't record its reviewer and decision time")
	}
	if n := h.count(t, `SELECT count(*) FROM business_profiles WHERE tenant_id = $1 AND verified_at IS NOT NULL`, tenant); n != 1 {
		t.Fatal("the business profile isn't marked verified")
	}
	for _, d := range docs {
		if got := h.documentStatus(t, d); got != domain.DocumentAccepted {
			t.Fatalf("document %s is %s, want accepted", d, got)
		}
	}
	data := assertEventValidWhere(t, h, "kyc.check_decided", tenant, "check_id", check.String())
	if data["status"] != domain.KYCPassed || data["promoted"] != true || data["reviewed_by"] != staff.String() {
		t.Fatalf("kyc.check_decided %v", data)
	}
	assertTierChangedEventValid(t, h, tenant)

	checks, err := h.svc.ListKYCChecks(ctx, admin, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if checks[0].ID != check || checks[0].Reason != "matches the registry" {
		t.Fatalf("the client sees %+v; want the decision's reason", checks[0])
	}

	t.Run("a second decision changes nothing", func(t *testing.T) {
		if _, err := h.svc.DecideKYCReview(ctx, h.newStaff(), check, domain.StaffDecision{Decision: domain.KYCFailed, Reason: "changed my mind"}); !errors.Is(err, app.ErrAlreadyDecided) {
			t.Fatalf("got %v, want ErrAlreadyDecided", err)
		}
		if got := h.checkStatus(t, check); got != domain.KYCPassed {
			t.Fatalf("the check is %s, want passed", got)
		}
		if n := h.count(t, `SELECT count(*) FROM tier_changes WHERE tenant_id = $1`, tenant); n != 2 {
			t.Fatalf("%d tier changes, want 2 (starter, then business)", n)
		}
	})
}

func TestDecideKYCReview_BusinessFail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	staff := h.newStaff()
	tenant, admin, check, docs := h.businessReview(t, "PVT-FAIL")
	taxCert := docs[1]

	t.Run("an invalid decision writes nothing", func(t *testing.T) {
		for _, d := range []domain.StaffDecision{
			{Decision: domain.KYCFailed, Reason: ""},
			{Decision: domain.KYCPassed, Reason: "ok", RejectedDocumentIDs: []uuid.UUID{taxCert}},
			{Decision: domain.KYCFailed, Reason: "x", RejectedDocumentIDs: []uuid.UUID{uuid.New()}},
		} {
			if _, err := h.svc.DecideKYCReview(ctx, staff, check, d); !errors.Is(err, domain.ErrInvalidDecision) {
				t.Fatalf("%+v: got %v, want ErrInvalidDecision", d, err)
			}
		}
		if got := h.checkStatus(t, check); got != domain.KYCReview {
			t.Fatalf("the check is %s, want review", got)
		}
	})

	if _, err := h.svc.DecideKYCReview(ctx, staff, check, domain.StaffDecision{
		Decision: domain.KYCFailed, Reason: "the tax certificate is expired", RejectedDocumentIDs: []uuid.UUID{taxCert},
	}); err != nil {
		t.Fatalf("DecideKYCReview: %v", err)
	}
	if got := h.tierOf(t, tenant); got != string(domain.TierStarter) {
		t.Fatalf("tier %s, want starter", got)
	}
	if got := h.documentStatus(t, taxCert); got != domain.DocumentRejected {
		t.Fatalf("the tax certificate is %s, want rejected", got)
	}
	if got := h.documentStatus(t, docs[0]); got != domain.DocumentReceived {
		t.Fatalf("an unrejected document is %s, want received", got)
	}
	if n := h.count(t, `SELECT count(*) FROM business_profiles WHERE tenant_id = $1 AND verified_at IS NOT NULL`, tenant); n != 0 {
		t.Fatal("a failed check verified the profile")
	}
	data := assertEventValidWhere(t, h, "kyc.check_decided", tenant, "check_id", check.String())
	if data["status"] != domain.KYCFailed || data["promoted"] != false {
		t.Fatalf("kyc.check_decided %v", data)
	}

	if _, err := h.svc.SubmitKYB(ctx, admin, tenant); !errors.Is(err, domain.ErrMissingDocuments) {
		t.Fatalf("resubmitting without a new tax certificate: got %v, want ErrMissingDocuments", err)
	}
	h.addDocument(t, tenant, domain.DocTaxCertificate, domain.DocumentReceived)
	if _, err := h.svc.SubmitKYB(ctx, admin, tenant); err != nil {
		t.Fatalf("resubmitting with a new tax certificate: %v", err)
	}
}

func TestDecideKYCReview_IndividualAndTheVendor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	staff := h.newStaff()

	t.Run("staff pass a vendor's review: starter, and the vendor's later result changes nothing", func(t *testing.T) {
		tenant, check, ref := h.startedCheck(t)
		if err := h.deliver(t, ref, domain.KYCReview, kycvendor.NoTamper); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.DecideKYCReview(ctx, staff, check, domain.StaffDecision{Decision: domain.KYCPassed, Reason: "selfie matches"}); err != nil {
			t.Fatalf("DecideKYCReview: %v", err)
		}
		if got := h.tierOf(t, tenant); got != string(domain.TierStarter) {
			t.Fatalf("tier %s, want starter", got)
		}
		if err := h.deliver(t, ref, domain.KYCFailed, kycvendor.NoTamper); err != nil {
			t.Fatal(err)
		}
		if got := h.checkStatus(t, check); got != domain.KYCPassed {
			t.Fatalf("the vendor overrode staff: %s", got)
		}
	})

	t.Run("the vendor decides first: staff get ErrAlreadyDecided", func(t *testing.T) {
		_, check, ref := h.startedCheck(t)
		if err := h.deliver(t, ref, domain.KYCReview, kycvendor.NoTamper); err != nil {
			t.Fatal(err)
		}
		if err := h.deliver(t, ref, domain.KYCFailed, kycvendor.NoTamper); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.DecideKYCReview(ctx, staff, check, domain.StaffDecision{Decision: domain.KYCPassed, Reason: "ok"}); !errors.Is(err, app.ErrAlreadyDecided) {
			t.Fatalf("got %v, want ErrAlreadyDecided", err)
		}
	})

	t.Run("a pending check isn't staff's to decide", func(t *testing.T) {
		_, check, _ := h.startedCheck(t)
		if _, err := h.svc.DecideKYCReview(ctx, staff, check, domain.StaffDecision{Decision: domain.KYCPassed, Reason: "ok"}); !errors.Is(err, app.ErrAlreadyDecided) {
			t.Fatalf("got %v, want ErrAlreadyDecided (not in review)", err)
		}
	})

	if _, err := h.svc.DecideKYCReview(ctx, staff, uuid.New(), domain.StaffDecision{Decision: domain.KYCPassed, Reason: "ok"}); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("an unknown check: got %v, want ErrNotFound", err)
	}
}

func TestStaffTierChangeAndReseller(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	staff := h.newStaff()
	tenant, admin, _ := h.starterTeam(t)

	if _, err := h.svc.StaffMakeReseller(ctx, staff, tenant, "agreement signed"); !errors.Is(err, domain.ErrResellerNeedsFinancial) {
		t.Fatalf("a reseller below financial: got %v", err)
	}
	if _, err := h.svc.StaffChangeTier(ctx, staff, tenant, domain.TierStarter, "again", nil); !errors.Is(err, domain.ErrSameTier) {
		t.Fatalf("the same tier: got %v, want ErrSameTier", err)
	}
	if _, err := h.svc.StaffChangeTier(ctx, staff, tenant, domain.TierFinancial, " ", nil); !errors.Is(err, domain.ErrNoReason) {
		t.Fatalf("no reason: got %v, want ErrNoReason", err)
	}
	if _, err := h.svc.StaffChangeTier(ctx, staff, uuid.New(), domain.TierFinancial, "x", nil); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("an unknown account: got %v, want ErrNotFound", err)
	}

	updated, err := h.svc.StaffChangeTier(ctx, staff, tenant, domain.TierFinancial, "v3 client with documents on file", nil)
	if err != nil {
		t.Fatalf("StaffChangeTier: %v", err)
	}
	if updated.Tier != domain.TierFinancial {
		t.Fatalf("returned tier %s, want financial", updated.Tier)
	}
	if n := h.count(t, `SELECT count(*) FROM tier_changes WHERE tenant_id = $1 AND to_tier = 'financial' AND changed_by = $2`, tenant, staff); n != 1 {
		t.Fatal("no tier_changes row naming the staff member")
	}

	reseller, err := h.svc.StaffMakeReseller(ctx, staff, tenant, "agreement signed")
	if err != nil {
		t.Fatalf("StaffMakeReseller: %v", err)
	}
	if reseller.Type != "reseller" {
		t.Fatalf("type %s, want reseller", reseller.Type)
	}
	if _, err := h.svc.CreateSubAccount(ctx, admin, tenant, app.SubAccountInput{DisplayName: "Client One"}); err != nil {
		t.Fatalf("the new reseller creates a sub-account: %v", err)
	}
}

func TestStaffDownloadDocument(t *testing.T) {
	h := newHarnessWithDocuments(t)
	ctx := context.Background()
	staff := h.newStaff()
	tenant, admin, _, _ := h.newTeam(t)
	original := pdf("certificate of incorporation")
	doc, _, err := h.svc.UploadDocument(ctx, admin, tenant, domain.DocIncorporationCert, original)
	if err != nil {
		t.Fatal(err)
	}
	data, mime, err := h.svc.StaffDownloadDocument(ctx, staff, tenant, doc.ID)
	if err != nil || string(data) != string(original) || mime != "application/pdf" {
		t.Fatalf("got %q %s %v; want the original PDF", data, mime, err)
	}
	other, _, _, _ := h.newTeam(t)
	if _, _, err := h.svc.StaffDownloadDocument(ctx, staff, other, doc.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("the document under another account: got %v, want ErrNotFound", err)
	}
}
