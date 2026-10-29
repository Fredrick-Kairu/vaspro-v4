package domain_test

import (
	"net/netip"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
)

func TestParseIPAllowlist_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		want    []netip.Prefix
		wantErr bool
	}{
		{
			name:    "empty list",
			entries: nil,
			want:    []netip.Prefix{},
		},
		{
			name:    "CIDR passes through",
			entries: []string{"203.0.113.0/24"},
			want:    []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		},
		{
			name:    "bare IPv4 becomes /32",
			entries: []string{"203.0.113.5"},
			want:    []netip.Prefix{netip.PrefixFrom(netip.MustParseAddr("203.0.113.5"), 32)},
		},
		{
			name:    "bare IPv6 becomes /128",
			entries: []string{"2001:db8::1"},
			want:    []netip.Prefix{netip.PrefixFrom(netip.MustParseAddr("2001:db8::1"), 128)},
		},
		{
			name:    "garbage is rejected",
			entries: []string{"not-an-ip"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.ParseIPAllowlist(tc.entries)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
