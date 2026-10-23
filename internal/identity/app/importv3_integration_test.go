//go:build integration

package app_test

import (
	"context"
	"crypto/md5" //nolint:gosec // building a v3-shaped test fixture hash, not production use
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// v3Hash reproduces v3's own documented scheme (data model §3: "v3
// passwords are bcrypt of MD5") as a test fixture — every test in
// this file needs a v3-shaped hash to import, never a real one.
func v3Hash(t *testing.T, password string) string {
	t.Helper()
	sum := md5.Sum([]byte(password)) //nolint:gosec // see v3Hash's own doc comment
	hash, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(sum[:])), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	return string(hash)
}

func TestImportV3Users_HappyPath(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newTestService(t)

	report, err := svc.ImportV3Users(ctx, []app.V3UserRecord{
		{V3ID: "1", EmailAddress: "ada@example.com", FullNames: "Ada Lovelace", Password: v3Hash(t, "ada-pw"), Msisdn: "0712345678"},
	})
	if err != nil {
		t.Fatalf("ImportV3Users: %v", err)
	}
	if report.Imported != 1 || report.Skipped != 0 || report.Flagged != 0 {
		t.Fatalf("got report %+v, want 1 imported, 0 skipped, 0 flagged", report)
	}
	if report.Outcomes[0].Status != "imported" {
		t.Fatalf("got outcome %+v, want status imported", report.Outcomes[0])
	}
}

func TestImportV3Users_NoUsableEmail_IsFlaggedNotImported(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newTestService(t)

	report, err := svc.ImportV3Users(ctx, []app.V3UserRecord{
		{V3ID: "1", EmailAddress: "", FullNames: "No Email", Password: v3Hash(t, "pw")},
		{V3ID: "2", EmailAddress: "not-an-email", FullNames: "Bad Email", Password: v3Hash(t, "pw")},
	})
	if err != nil {
		t.Fatalf("ImportV3Users: %v", err)
	}
	if report.Imported != 0 || report.Flagged != 2 {
		t.Fatalf("got report %+v, want 0 imported, 2 flagged", report)
	}
	for _, o := range report.Outcomes {
		if o.Status != "flagged" {
			t.Fatalf("got outcome %+v, want status flagged", o)
		}
	}
}

func TestImportV3Users_DuplicateEmailWithinBatch_SecondIsSkipped(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newTestService(t)

	report, err := svc.ImportV3Users(ctx, []app.V3UserRecord{
		{V3ID: "1", EmailAddress: "ada@example.com", FullNames: "Ada", Password: v3Hash(t, "pw1")},
		{V3ID: "2", EmailAddress: "Ada@Example.com", FullNames: "Ada Duplicate", Password: v3Hash(t, "pw2")},
	})
	if err != nil {
		t.Fatalf("ImportV3Users: %v", err)
	}
	if report.Imported != 1 || report.Skipped != 1 {
		t.Fatalf("got report %+v, want 1 imported, 1 skipped", report)
	}
}

func TestImportV3Users_RerunIsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newTestService(t)
	records := []app.V3UserRecord{
		{V3ID: "1", EmailAddress: "ada@example.com", FullNames: "Ada", Password: v3Hash(t, "pw")},
	}

	first, err := svc.ImportV3Users(ctx, records)
	if err != nil {
		t.Fatalf("first ImportV3Users: %v", err)
	}
	if first.Imported != 1 {
		t.Fatalf("got first report %+v, want 1 imported", first)
	}

	second, err := svc.ImportV3Users(ctx, records)
	if err != nil {
		t.Fatalf("second ImportV3Users: %v", err)
	}
	if second.Imported != 0 || second.Skipped != 1 {
		t.Fatalf("got second report %+v, want 0 imported, 1 skipped (already imported)", second)
	}
}

func TestImportV3Users_UnrecognizedMsisdn_StillImportsWithoutPhone(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newTestService(t)

	report, err := svc.ImportV3Users(ctx, []app.V3UserRecord{
		{V3ID: "1", EmailAddress: "ada@example.com", FullNames: "Ada", Password: v3Hash(t, "pw"), Msisdn: "not-a-number"},
	})
	if err != nil {
		t.Fatalf("ImportV3Users: %v", err)
	}
	if report.Imported != 1 {
		t.Fatalf("got report %+v, want 1 imported (unparseable phone shouldn't block import)", report)
	}
}
