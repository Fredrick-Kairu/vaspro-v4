package main

import (
	"os"
	"strings"
	"testing"
)

func generateFromFile(t *testing.T, path string) (string, error) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-only, path is always a fixture constant passed by the test itself
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	out, err := Generate(data)
	return string(out), err
}

const validYAML = `
codes:
  - code: AUTH-4010
    status: 401
    title: Invalid or missing credentials
    cause: Bad token.
    fix: Sign in again.
    retryable: false
    billable: false
  - code: SYS-9000
    status: 500
    title: Internal error
    cause: Unexpected error.
    fix: Retry.
    retryable: true
    billable: false
`

func TestGenerate_ValidCatalogue(t *testing.T) {
	out, err := Generate([]byte(validYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	src := string(out)

	for _, want := range []string{
		"// Code generated",
		"package problem",
		`AUTH4010`,
		`"AUTH-4010"`,
		`SYS9000`,
		`"SYS-9000"`,
		`"Invalid or missing credentials"`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q\n--- source ---\n%s", want, src)
		}
	}
}

func TestGenerate_DuplicateCodeErrors(t *testing.T) {
	const yaml = `
codes:
  - code: AUTH-4010
    status: 401
    title: One
    cause: c
    fix: f
    retryable: false
    billable: false
  - code: AUTH-4010
    status: 402
    title: Two
    cause: c
    fix: f
    retryable: false
    billable: false
`
	_, err := Generate([]byte(yaml))
	if err == nil {
		t.Fatal("expected an error for a duplicate code")
	}
	if !strings.Contains(err.Error(), "AUTH-4010") {
		t.Fatalf("expected the error to name the duplicate code, got: %v", err)
	}
}

func TestGenerate_MalformedCodeErrors(t *testing.T) {
	const yaml = `
codes:
  - code: not-a-valid-code
    status: 400
    title: Bad
    cause: c
    fix: f
    retryable: false
    billable: false
`
	_, err := Generate([]byte(yaml))
	if err == nil {
		t.Fatal("expected an error for a malformed code")
	}
	if !strings.Contains(err.Error(), "not-a-valid-code") {
		t.Fatalf("expected the error to name the malformed code, got: %v", err)
	}
}

func TestGenerate_InvalidHTTPStatusErrors(t *testing.T) {
	const yaml = `
codes:
  - code: AUTH-4010
    status: 999
    title: Bad
    cause: c
    fix: f
    retryable: false
    billable: false
`
	_, err := Generate([]byte(yaml))
	if err == nil {
		t.Fatal("expected an error for an out-of-range HTTP status")
	}
}

func TestGenerate_RealCatalogue(t *testing.T) {
	// This is the actual contract file platform/problem is generated
	// from; make sure it parses and generates cleanly, not just the
	// synthetic fixtures above.
	data, err := generateFromFile(t, "../../docs/errors/catalogue.yaml")
	if err != nil {
		t.Fatalf("unexpected error generating from docs/errors/catalogue.yaml: %v", err)
	}
	if !strings.Contains(data, "SYS9000") {
		t.Fatal("expected SYS9000 to be present in the real catalogue")
	}
}
