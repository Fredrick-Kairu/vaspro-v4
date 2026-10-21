package main

import (
	"bytes"
	"strings"
	"testing"
)

const (
	realSchemaPath = "../../packs/schema.json"
	realKEPackPath = "../../packs/ke/pack.yaml"
)

func TestPacksValidate_RealKEPack_Passes(t *testing.T) {
	var out bytes.Buffer
	ok := packsValidate(&out, realSchemaPath, []string{realKEPackPath})
	if !ok {
		t.Fatalf("expected the real KE pack to validate, output: %s", out.String())
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("expected a pass line, got: %s", out.String())
	}
}

func TestPacksValidate_BrokenPack_FailsAndNamesIt(t *testing.T) {
	var out bytes.Buffer
	ok := packsValidate(&out, realSchemaPath, []string{"../../internal/platform/packs/testdata/missing_currency.yaml"})
	if ok {
		t.Fatal("expected a broken pack to fail validation")
	}
	if !strings.Contains(out.String(), "missing_currency.yaml") {
		t.Fatalf("expected the failure output to name the broken pack, got: %s", out.String())
	}
}

func TestPacksValidate_MixedPacks_ReportsEachAndFailsOverall(t *testing.T) {
	var out bytes.Buffer
	ok := packsValidate(&out, realSchemaPath, []string{
		realKEPackPath,
		"../../internal/platform/packs/testdata/missing_currency.yaml",
	})
	if ok {
		t.Fatal("expected overall failure when any pack is invalid")
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("expected the valid pack to still be reported ok, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("expected the invalid pack to be reported FAIL, got: %s", out.String())
	}
}

func TestDiscoverPacks_FindsTheKEPack(t *testing.T) {
	found, err := discoverPacks("../..")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var sawKE bool
	for _, f := range found {
		if strings.Contains(f, "ke") {
			sawKE = true
		}
	}
	if !sawKE {
		t.Fatalf("expected to find the KE pack among %v", found)
	}
}
