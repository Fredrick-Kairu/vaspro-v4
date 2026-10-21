// Package packs loads and validates country packs (data model D-09;
// ROADMAP Stage A item A6): versioned, per-country rules, never in code.
package packs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

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
	Numbering   Numbering
	Tax         Tax
	SendWindows SendWindows
}

type Numbering struct {
	CountryCode                     string
	TrunkPrefix                     string
	NationalSignificantNumberLength int
}

type Tax struct {
	VATRateBasisPoints    money.VATRateBasisPoints
	VATInclusiveByDefault bool
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
		VATRateBasisPoints    int64 `json:"vat_rate_basis_points"`
		VATInclusiveByDefault bool  `json:"vat_inclusive_by_default"`
	} `json:"tax"`
	SendWindows struct {
		Promotional struct {
			Start    string `json:"start"`
			End      string `json:"end"`
			Timezone string `json:"timezone"`
		} `json:"promotional"`
	} `json:"send_windows"`
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

	return &Pack{
		Version:     jp.Version,
		CountryCode: jp.CountryCode,
		Currency:    money.Currency{Code: jp.Currency.Code, Exponent: jp.Currency.Exponent},
		Numbering: Numbering{
			CountryCode:                     jp.Numbering.CountryCode,
			TrunkPrefix:                     jp.Numbering.TrunkPrefix,
			NationalSignificantNumberLength: jp.Numbering.NationalSignificantNumberLength,
		},
		Tax: Tax{
			VATRateBasisPoints:    money.VATRateBasisPoints(jp.Tax.VATRateBasisPoints),
			VATInclusiveByDefault: jp.Tax.VATInclusiveByDefault,
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
