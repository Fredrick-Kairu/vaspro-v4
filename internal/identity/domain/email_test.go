package domain

import "testing"

func TestNormalizeEmail_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"already lowercase", "foo@example.com", "foo@example.com", false},
		{"mixed case lowercased", "Foo.Bar@Example.COM", "foo.bar@example.com", false},
		{"surrounding whitespace trimmed", "  foo@example.com  ", "foo@example.com", false},
		{"display name rejected", "Foo Bar <foo@example.com>", "", true},
		{"empty string rejected", "", "", true},
		{"missing @ rejected", "not-an-email", "", true},
		{"multiple addresses rejected", "foo@example.com, bar@example.com", "", true},
		{"missing domain rejected", "foo@", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeEmail(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("NormalizeEmail(%q) = %q, nil; want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeEmail(%q): unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("NormalizeEmail(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func FuzzNormalizeEmail(f *testing.F) {
	for _, seed := range []string{
		"foo@example.com",
		"Foo@Example.COM",
		"",
		"not-an-email",
		"foo@",
		"<script>@example.com",
		"foo@example.com, bar@example.com",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		got, err := NormalizeEmail(raw)
		if err != nil {
			return // rejecting is always a valid outcome
		}
		// Idempotent: normalizing an already-normalized address must
		// return it unchanged.
		again, err := NormalizeEmail(got)
		if err != nil {
			t.Fatalf("NormalizeEmail(%q) succeeded but normalizing its own output %q failed: %v", raw, got, err)
		}
		if again != got {
			t.Fatalf("NormalizeEmail not idempotent: NormalizeEmail(%q) = %q, but NormalizeEmail(%q) = %q", raw, got, got, again)
		}
	})
}
