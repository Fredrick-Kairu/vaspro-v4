package domain

import "testing"

func TestNormalizeV3MSISDN(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantE164 string
		wantOK   bool
	}{
		{"already E.164", "+254712345678", "+254712345678", true},
		{"leading zero, local format", "0712345678", "+254712345678", true},
		{"leading 254, no plus", "254712345678", "+254712345678", true},
		{"airtel/telkom range (leading 1)", "0112345678", "+254112345678", true},
		{"with spaces", "0712 345 678", "+254712345678", true},
		{"with dashes", "0712-345-678", "+254712345678", true},
		{"wrong subscriber prefix", "0212345678", "", false},
		{"too short", "071234", "", false},
		{"garbage", "not-a-number", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeV3MSISDN(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("NormalizeV3MSISDN(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
			}
			if ok && got != tc.wantE164 {
				t.Fatalf("NormalizeV3MSISDN(%q) = %q, want %q", tc.raw, got, tc.wantE164)
			}
		})
	}
}
