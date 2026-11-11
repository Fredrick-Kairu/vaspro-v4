// Package packs loads and validates country packs (data model D-09;
// ROADMAP Stage A item A6): versioned, per-country rules, never in code.
package packs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/money"
)

// Pack is a validated country pack. Only the fields task 0.10 needs are
// here; packs/schema.json already reserves operators/routes/rails/
// compliance_keywords/test_numbers for later tasks to add without a
// schema change — Pack grows to match once something reads them.
type Pack struct {
	Version     string
	CountryCode string
	Currency    money.Currency
	// Timezone and DataRegion are a new account's defaults (task 2.2a):
	// accounts.tenants.timezone and .region (data model §4).
	Timezone    string
	DataRegion  string
	Numbering   Numbering
	Tax         Tax
	SendWindows SendWindows
	// Services is each service's minimum tier and availability here,
	// keyed by accounts' service code (task 2.4).
	Services map[string]ServiceRule
	// KYB is business verification here (task 2.7); nil when the
	// country can't do KYB yet.
	KYB *KYB
	// Ledger is the ledger's country rules (task 3.7).
	Ledger Ledger
	// Rails are the country's payment rails (task 4.3).
	Rails []Rail
}

// Rail is one payment rail in a country (data model §7's payments.rails).
type Rail struct {
	Code         string
	Kind         string
	Capabilities []string
	Status       string
	// VASPROTopups: clients top up on VASPRO's own paybill on this rail.
	VASPROTopups bool
}

// TopupRail is the rail clients top up on here, if the country has one.
func (p *Pack) TopupRail() (Rail, bool) {
	for _, r := range p.Rails {
		if r.VASPROTopups {
			return r, true
		}
	}
	return Rail{}, false
}

// Ledger is a country's ledger rules.
type Ledger struct {
	// ReversalFinanceLeadMinor: a reversal of at least this many minor
	// units needs a finance lead (OQ-18); nil means every reversal does.
	ReversalFinanceLeadMinor *int64
}

// KYB is a country's business verification rule.
type KYB struct {
	Registry          string   // BRS, BRELA, URSB
	TaxIDLabel        string   // KRA PIN, TIN
	RequiredDocuments []string // document types KYB needs uploaded (task 2.8)
}

// ServiceRule is one service's rule in a country.
type ServiceRule struct {
	MinTier string
	Enabled bool
}

type Numbering struct {
	CountryCode                     string
	TrunkPrefix                     string
	NationalSignificantNumberLength int
}

type Tax struct {
	VATRateBasisPoints    money.VATRateBasisPoints
	VATInclusiveByDefault bool
	// VATEffectiveFrom is the date the rate applies from (task 3.5),
	// midnight UTC.
	VATEffectiveFrom time.Time
}

type SendWindow struct {
	Start    string
	End      string
	Timezone string
}

type SendWindows struct {
	Promotional SendWindow
}

// jsonPack mirrors packs/schema.json's shape for decoding; Parse decodes
// into this only after schema validation has already passed, so its
// fields can assume the shape is correct.
type jsonPack struct {
	Version     string `json:"version"`
	CountryCode string `json:"country_code"`
	Timezone    string `json:"timezone"`
	DataRegion  string `json:"data_region"`
	Currency    struct {
		Code     string `json:"code"`
		Exponent int    `json:"exponent"`
	} `json:"currency"`
	Numbering struct {
		CountryCode                     string `json:"country_code"`
		TrunkPrefix                     string `json:"trunk_prefix"`
		NationalSignificantNumberLength int    `json:"national_significant_number_length"`
	} `json:"numbering"`
	Tax struct {
		VATRateBasisPoints    int64  `json:"vat_rate_basis_points"`
		VATInclusiveByDefault bool   `json:"vat_inclusive_by_default"`
		VATEffectiveFrom      string `json:"vat_effective_from"`
	} `json:"tax"`
	SendWindows struct {
		Promotional struct {
			Start    string `json:"start"`
			End      string `json:"end"`
			Timezone string `json:"timezone"`
		} `json:"promotional"`
	} `json:"send_windows"`
	KYB *struct {
		Registry          string   `json:"registry"`
		TaxIDLabel        string   `json:"tax_id_label"`
		RequiredDocuments []string `json:"required_documents"`
	} `json:"kyb"`
	Ledger struct {
		ReversalFinanceLeadMinor *int64 `json:"reversal_finance_lead_minor"`
	} `json:"ledger"`
	Services map[string]struct {
		MinTier string `json:"min_tier"`
		Enabled *bool  `json:"enabled"`
	} `json:"services"`
	Rails []struct {
		Code         string   `json:"code"`
		Kind         string   `json:"kind"`
		Capabilities []string `json:"capabilities"`
		Status       string   `json:"status"`
		VASPROTopups bool     `json:"vaspro_topups"`
	} `json:"rails"`
}

// Parse validates packYAML against schemaJSON and decodes it into a
// Pack. Pure function over bytes, so it's testable without touching the
// filesystem (same shape as tools/errcodes.Generate).
func Parse(schemaJSON, packYAML []byte) (*Pack, error) {
	packJSON, err := yaml.YAMLToJSON(packYAML)
	if err != nil {
		return nil, fmt.Errorf("packs: pack is not valid YAML: %w", err)
	}

	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("packs: schema is not valid JSON: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("pack-schema.json", schemaDoc); err != nil {
		return nil, fmt.Errorf("packs: loading schema: %w", err)
	}
	schema, err := compiler.Compile("pack-schema.json")
	if err != nil {
		return nil, fmt.Errorf("packs: compiling schema: %w", err)
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(packJSON))
	if err != nil {
		return nil, fmt.Errorf("packs: pack is not valid JSON after YAML conversion: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return nil, fmt.Errorf("packs: pack failed schema validation: %w", err)
	}

	var jp jsonPack
	if err := json.Unmarshal(packJSON, &jp); err != nil {
		return nil, fmt.Errorf("packs: decoding validated pack: %w", err)
	}

	services := make(map[string]ServiceRule, len(jp.Services))
	for code, r := range jp.Services {
		services[code] = ServiceRule{MinTier: r.MinTier, Enabled: r.Enabled == nil || *r.Enabled}
	}

	vatFrom, err := time.Parse(time.DateOnly, jp.Tax.VATEffectiveFrom)
	if err != nil {
		return nil, fmt.Errorf("packs: tax.vat_effective_from %q is not a date: %w", jp.Tax.VATEffectiveFrom, err)
	}

	rails := make([]Rail, len(jp.Rails))
	topups := 0
	for i, r := range jp.Rails {
		rails[i] = Rail{Code: r.Code, Kind: r.Kind, Capabilities: r.Capabilities, Status: r.Status, VASPROTopups: r.VASPROTopups}
		if r.VASPROTopups {
			topups++
		}
	}
	if topups > 1 {
		return nil, fmt.Errorf("packs: %d rails carry VASPRO's top-ups; at most one may", topups)
	}

	var kyb *KYB
	if jp.KYB != nil {
		kyb = &KYB{Registry: jp.KYB.Registry, TaxIDLabel: jp.KYB.TaxIDLabel, RequiredDocuments: jp.KYB.RequiredDocuments}
	}

	return &Pack{
		Ledger:      Ledger{ReversalFinanceLeadMinor: jp.Ledger.ReversalFinanceLeadMinor},
		Rails:       rails,
		KYB:         kyb,
		Services:    services,
		Version:     jp.Version,
		CountryCode: jp.CountryCode,
		Currency:    money.Currency{Code: jp.Currency.Code, Exponent: jp.Currency.Exponent},
		Timezone:    jp.Timezone,
		DataRegion:  jp.DataRegion,
		Numbering: Numbering{
			CountryCode:                     jp.Numbering.CountryCode,
			TrunkPrefix:                     jp.Numbering.TrunkPrefix,
			NationalSignificantNumberLength: jp.Numbering.NationalSignificantNumberLength,
		},
		Tax: Tax{
			VATRateBasisPoints:    money.VATRateBasisPoints(jp.Tax.VATRateBasisPoints),
			VATInclusiveByDefault: jp.Tax.VATInclusiveByDefault,
			VATEffectiveFrom:      vatFrom,
		},
		SendWindows: SendWindows{
			Promotional: SendWindow{
				Start:    jp.SendWindows.Promotional.Start,
				End:      jp.SendWindows.Promotional.End,
				Timezone: jp.SendWindows.Promotional.Timezone,
			},
		},
	}, nil
}

// Load reads schemaPath and packPath from disk and calls Parse.
func Load(schemaPath, packPath string) (*Pack, error) {
	schemaJSON, err := os.ReadFile(schemaPath) //nolint:gosec // schemaPath is a caller-supplied config value (e.g. "packs/schema.json"), not untrusted input
	if err != nil {
		return nil, fmt.Errorf("packs: reading schema %s: %w", schemaPath, err)
	}
	packYAML, err := os.ReadFile(packPath) //nolint:gosec // same: packPath is a caller-supplied config value
	if err != nil {
		return nil, fmt.Errorf("packs: reading pack %s: %w", packPath, err)
	}
	return Parse(schemaJSON, packYAML)
}

// LoadPinned is Load, then errors unless the pack's own Version equals
// wantVersion — a service pins the exact pack version it was built and
// tested against, so an unrelated pack upgrade can't change its
// behavior silently underneath it.
func LoadPinned(schemaPath, packPath, wantVersion string) (*Pack, error) {
	p, err := Load(schemaPath, packPath)
	if err != nil {
		return nil, err
	}
	if p.Version != wantVersion {
		return nil, fmt.Errorf("packs: %s: version pin mismatch: want %q, got %q", packPath, wantVersion, p.Version)
	}
	return p, nil
}
