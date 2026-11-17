package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

func TestValidAttributeKey(t *testing.T) {
	for key, want := range map[string]bool{
		"county": true, "loyalty_tier2": true, "a": true, "a" + strings.Repeat("b", 39): true,
		"": false, "County": false, "2county": false, "home-county": false, "a.b": false, "a b": false,
		"a" + strings.Repeat("b", 40): false, "_x": false,
	} {
		if got := domain.ValidAttributeKey(key); got != want {
			t.Errorf("ValidAttributeKey(%q) = %v", key, got)
		}
	}
}

// TestAttributeValue: what each type accepts, and the JSON it stores.
// Numbers stay decimal text in JSON, never a float.
func TestAttributeValue(t *testing.T) {
	for _, tc := range []struct {
		typ, in, want string
	}{
		{"text", "Nairobi", `"Nairobi"`},
		{"text", "  Nairobi  ", `"Nairobi"`},
		{"text", `say "hi"`, `"say \"hi\""`},
		{"number", "42", `42`},
		{"number", "-3.50", `-3.50`},
		{"number", " 1000000 ", `1000000`},
		{"date", "2026-11-17", `"2026-11-17"`},
		{"boolean", "true", `true`},
		{"boolean", "FALSE", `false`},
		{"boolean", "Yes", `true`},
		{"boolean", "no", `false`},
	} {
		got, err := domain.AttributeValue(tc.typ, tc.in)
		if err != nil || string(got) != tc.want {
			t.Errorf("AttributeValue(%s, %q) = %s, %v; want %s", tc.typ, tc.in, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ typ, in string }{
		{"text", strings.Repeat("x", 256)},
		{"number", "1e9"}, {"number", "1,000"}, {"number", "NaN"}, {"number", "0x10"}, {"number", "1."}, {"number", strings.Repeat("9", 40)},
		{"date", "17/11/2026"}, {"date", "2026-02-30"}, {"date", "2026-11-17T00:00:00Z"},
		{"boolean", "maybe"}, {"boolean", "2"},
		{"json", "{}"},
	} {
		if _, err := domain.AttributeValue(tc.typ, tc.in); !errors.Is(err, domain.ErrValueInvalid) {
			t.Errorf("AttributeValue(%s, %q): %v, want ErrValueInvalid", tc.typ, tc.in, err)
		}
	}
	// An empty cell is "no value", not an error.
	if got, err := domain.AttributeValue("number", "   "); err != nil || got != nil {
		t.Errorf("an empty number: %s, %v", got, err)
	}
}

func TestContactFields(t *testing.T) {
	ok := domain.ContactFields{FirstName: " Ann ", LastName: "Test", Email: " Ann@Example.com "}
	got, err := domain.NormalizeContactFields(ok)
	if err != nil || got.FirstName != "Ann" || got.Email != "Ann@Example.com" {
		t.Fatalf("%+v, %v", got, err)
	}
	for name, f := range map[string]domain.ContactFields{
		"a long first name":    {FirstName: strings.Repeat("a", 101)},
		"a long last name":     {LastName: strings.Repeat("a", 101)},
		"an email with no @":   {Email: "ann.example.com"},
		"an email with a name": {Email: "Ann <ann@example.com>"},
		"a long email":         {Email: strings.Repeat("a", 250) + "@x.io"},
		"a control character":  {FirstName: "Ann\x00"},
	} {
		if _, err := domain.NormalizeContactFields(f); !errors.Is(err, domain.ErrValueInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
