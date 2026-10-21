package domain

import "testing"

func TestValidatePhoneE164(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"valid kenyan number", "+254700000000", "+254700000000", false},
		{"surrounding whitespace trimmed", "  +254700000000  ", "+254700000000", false},
		{"missing plus rejected", "254700000000", "", true},
		{"leading zero after plus rejected", "+0700000000", "", true},
		{"too short rejected", "+2547", "", true},
		{"contains letters rejected", "+25470000000a", "", true},
		{"empty rejected", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidatePhoneE164(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ValidatePhoneE164(%q) = %q, nil; want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidatePhoneE164(%q): unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ValidatePhoneE164(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
