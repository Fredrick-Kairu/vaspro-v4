package packs_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

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

// TestParse_DataRegion_OnlyAHostedRegion covers task 2.10: a pack can
// only start accounts in a region the schema lists as hosted.
func TestParse_DataRegion_OnlyAHostedRegion(t *testing.T) {
	schema := readFile(t, realSchemaPath)
	ke := string(readFile(t, realKEPackPath))
	for _, region := range []string{"mars", "KE", ""} {
		pack := strings.Replace(ke, "\ndata_region: ke\n", "\ndata_region: \""+region+"\"\n", 1)
		if pack == ke {
			t.Fatal("the KE pack no longer has the line this test replaces")
		}
		if _, err := packs.Parse(schema, []byte(pack)); err == nil {
			t.Errorf("data_region %q was accepted", region)
		}
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

// TestParse_VATEffectiveFrom covers task 3.5 (OQ-16): the VAT rate
// carries the date it applies from, so the ledger's tax_rates hold its
// history; a pack without one, or with a malformed one, is refused.
func TestParse_VATEffectiveFrom(t *testing.T) {
	schema := readFile(t, realSchemaPath)
	ke := string(readFile(t, realKEPackPath))
	p, err := packs.Parse(schema, []byte(ke))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); !p.Tax.VATEffectiveFrom.Equal(want) {
		t.Errorf("VATEffectiveFrom = %s, want %s", p.Tax.VATEffectiveFrom, want)
	}
	const line = "\n  vat_effective_from: \"2026-01-01\"\n"
	if !strings.Contains(ke, line) {
		t.Fatal("the KE pack no longer has the line this test replaces")
	}
	for name, replacement := range map[string]string{
		"missing":       "\n",
		"not a date":    "\n  vat_effective_from: \"soon\"\n",
		"a date-time":   "\n  vat_effective_from: \"2026-01-01T00:00:00Z\"\n",
		"an impossible": "\n  vat_effective_from: \"2026-02-30\"\n",
	} {
		if _, err := packs.Parse(schema, []byte(strings.Replace(ke, line, replacement, 1))); err == nil {
			t.Errorf("%s vat_effective_from was accepted", name)
		}
	}
}

// The finance-lead threshold for reversals is a country rule (task 3.7,
// OQ-18): KES 2,000 in Kenya. A pack without one leaves it nil, which
// the ledger reads as "every reversal needs a finance lead".
func TestParse_ReversalFinanceLeadThreshold(t *testing.T) {
	schema := readFile(t, realSchemaPath)
	p, err := packs.Parse(schema, readFile(t, realKEPackPath))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Ledger.ReversalFinanceLeadMinor == nil || *p.Ledger.ReversalFinanceLeadMinor != 200000 {
		t.Fatalf("KE reversal threshold = %v, want 200000 (KES 2,000)", p.Ledger.ReversalFinanceLeadMinor)
	}

	base := string(readFile(t, realKEPackPath))
	i := strings.Index(base, "\nledger:")
	if i < 0 {
		t.Fatal("KE pack has no ledger block")
	}
	without := base[:i] + base[i+len("\nledger:\n  reversal_finance_lead_minor: 200000"):]
	p, err = packs.Parse(schema, []byte(without))
	if err != nil {
		t.Fatalf("a pack without a ledger block: %v", err)
	}
	if p.Ledger.ReversalFinanceLeadMinor != nil {
		t.Fatalf("threshold = %v with no ledger block, want nil", *p.Ledger.ReversalFinanceLeadMinor)
	}

	for _, bad := range []string{"0", "-5", "12.5", `"2000"`} {
		if _, err := packs.Parse(schema, []byte(strings.Replace(base, "reversal_finance_lead_minor: 200000", "reversal_finance_lead_minor: "+bad, 1))); err == nil {
			t.Errorf("threshold %s accepted, want refused", bad)
		}
	}
}
