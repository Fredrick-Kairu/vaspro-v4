package packs_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/money"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
)

const (
	realSchemaPath = "../../../packs/schema.json"
	realKEPackPath = "../../../packs/ke/pack.yaml"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-only, path is always a fixture constant passed by the test itself
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return data
}

func TestParse_RealKEPack_ParsesCleanly(t *testing.T) {
	schema := readFile(t, realSchemaPath)
	pack := readFile(t, realKEPackPath)

	p, err := packs.Parse(schema, pack)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p.Version != "0" {
		t.Errorf("Version = %q, want %q", p.Version, "0")
	}
	if p.CountryCode != "KE" {
		t.Errorf("CountryCode = %q, want %q", p.CountryCode, "KE")
	}
	if p.Currency != money.KES {
		t.Errorf("Currency = %+v, want %+v", p.Currency, money.KES)
	}
	if p.Numbering.CountryCode != "254" || p.Numbering.TrunkPrefix != "0" || p.Numbering.NationalSignificantNumberLength != 9 {
		t.Errorf("Numbering = %+v, unexpected", p.Numbering)
	}
	if p.Tax.VATRateBasisPoints != 1600 || !p.Tax.VATInclusiveByDefault {
		t.Errorf("Tax = %+v, unexpected", p.Tax)
	}
	if p.SendWindows.Promotional.Start != "08:00" || p.SendWindows.Promotional.End != "20:00" || p.SendWindows.Promotional.Timezone != "Africa/Nairobi" {
		t.Errorf("SendWindows.Promotional = %+v, unexpected", p.SendWindows.Promotional)
	}
}

// A new account takes its time zone and data residency tag from its
// country's pack (task 2.2a).
func TestParse_RealKEPack_HasTenantDefaults(t *testing.T) {
	p, err := packs.Parse(readFile(t, realSchemaPath), readFile(t, realKEPackPath))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Timezone != "Africa/Nairobi" || p.DataRegion != "ke" {
		t.Errorf("Timezone = %q, DataRegion = %q, want Africa/Nairobi and ke", p.Timezone, p.DataRegion)
	}
}

// Each service's minimum tier and availability in the country (task
// 2.4, OQ-06): KE lists the requirements' tier table.
func TestParse_RealKEPack_HasServiceRules(t *testing.T) {
	p, err := packs.Parse(readFile(t, realSchemaPath), readFile(t, realKEPackPath))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		"sms": "starter", "surveys": "starter", "ussd": "starter",
		"whatsapp": "business", "rewards": "business",
		"payouts": "financial", "collections": "financial",
	}
	if len(p.Services) != len(want) {
		t.Fatalf("got %d services, want %d: %+v", len(p.Services), len(want), p.Services)
	}
	for code, tier := range want {
		rule, ok := p.Services[code]
		if !ok || rule.MinTier != tier || !rule.Enabled {
			t.Errorf("%s: got %+v, want min tier %s, enabled", code, rule, tier)
		}
	}
}

// KYB's registry and tax ID name are country rules (task 2.7).
func TestParse_RealKEPack_HasKYB(t *testing.T) {
	p, err := packs.Parse(readFile(t, realSchemaPath), readFile(t, realKEPackPath))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.KYB == nil || p.KYB.Registry != "BRS" || p.KYB.TaxIDLabel != "KRA PIN" {
		t.Fatalf("KYB = %+v, want BRS and KRA PIN", p.KYB)
	}
	if want := []string{"incorporation_cert", "tax_certificate", "proof_of_address"}; !slices.Equal(p.KYB.RequiredDocuments, want) {
		t.Fatalf("required documents %v, want %v (task 2.8)", p.KYB.RequiredDocuments, want)
	}
}

func TestParse_ServiceRule_EnabledDefaultsToTrueAndTierIsChecked(t *testing.T) {
	schema := readFile(t, realSchemaPath)
	base := string(readFile(t, realKEPackPath))
	base = base[:strings.Index(base, "\nservices:")]

	p, err := packs.Parse(schema, []byte(base+"\nservices:\n  sms:\n    min_tier: business\n  ussd:\n    min_tier: starter\n    enabled: false\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r := p.Services["sms"]; r.MinTier != "business" || !r.Enabled {
		t.Errorf("sms: %+v, want business and enabled by default", r)
	}
	if r := p.Services["ussd"]; r.Enabled {
		t.Errorf("ussd: %+v, want disabled", r)
	}
	if _, err := packs.Parse(schema, []byte(base+"\nservices:\n  sms:\n    min_tier: platinum\n")); err == nil {
		t.Error("a tier that doesn't exist was accepted")
	}
}

func TestParse_MissingRequiredField_RejectedWithClearError(t *testing.T) {
	schema := readFile(t, realSchemaPath)
	pack := readFile(t, "testdata/missing_currency.yaml")

	_, err := packs.Parse(schema, pack)
	if err == nil {
		t.Fatal("expected an error for a pack missing a required field")
	}
	if !strings.Contains(err.Error(), "currency") {
		t.Fatalf("expected the error to name the missing field 'currency', got: %v", err)
	}
}

func TestParse_WrongType_Rejected(t *testing.T) {
	schema := readFile(t, realSchemaPath)
	pack := readFile(t, "testdata/wrong_type.yaml")

	_, err := packs.Parse(schema, pack)
	if err == nil {
		t.Fatal("expected an error for a field with the wrong type")
	}
}

func TestParse_MalformedYAML_ErrorsWithoutPanic(t *testing.T) {
	schema := readFile(t, realSchemaPath)
	pack := readFile(t, "testdata/malformed.yaml")

	_, err := packs.Parse(schema, pack)
	if err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
}

func TestParse_InvalidSchemaJSON_Errors(t *testing.T) {
	_, err := packs.Parse([]byte("{not valid json"), readFile(t, realKEPackPath))
	if err == nil {
		t.Fatal("expected an error for an invalid schema document")
	}
}

func TestLoad_ReadsFromDisk(t *testing.T) {
	p, err := packs.Load(realSchemaPath, realKEPackPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.CountryCode != "KE" {
		t.Fatalf("CountryCode = %q, want KE", p.CountryCode)
	}
}

func TestLoad_MissingFile_Errors(t *testing.T) {
	_, err := packs.Load(realSchemaPath, "testdata/does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected an error for a missing pack file")
	}
}

func TestLoadPinned_CorrectVersion_Succeeds(t *testing.T) {
	p, err := packs.LoadPinned(realSchemaPath, realKEPackPath, "0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Version != "0" {
		t.Fatalf("Version = %q, want %q", p.Version, "0")
	}
}

func TestLoadPinned_WrongVersion_FailsWithBothVersionsNamed(t *testing.T) {
	_, err := packs.LoadPinned(realSchemaPath, realKEPackPath, "99")
	if err == nil {
		t.Fatal("expected an error when the wanted version doesn't match")
	}
	if !strings.Contains(err.Error(), "99") || !strings.Contains(err.Error(), "0") {
		t.Fatalf("expected the error to name both the wanted (99) and actual (0) versions, got: %v", err)
	}
}
