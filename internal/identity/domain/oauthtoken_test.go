package domain_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
)

func TestGrantedScopes_TableDriven(t *testing.T) {
	cases := []struct {
		name         string
		clientScopes []string
		requested    string
		want         []string
		wantErr      bool
	}{
		{
			name:         "empty request grants every client scope",
			clientScopes: []string{"sms:send", "reports:read"},
			requested:    "",
			want:         []string{"sms:send", "reports:read"},
		},
		{
			name:         "whitespace-only request grants every client scope",
			clientScopes: []string{"sms:send"},
			requested:    "   ",
			want:         []string{"sms:send"},
		},
		{
			name:         "a subset is granted exactly",
			clientScopes: []string{"sms:send", "reports:read"},
			requested:    "reports:read",
			want:         []string{"reports:read"},
		},
		{
			name:         "requested order doesn't matter, client order wins",
			clientScopes: []string{"sms:send", "reports:read"},
			requested:    "reports:read sms:send",
			want:         []string{"sms:send", "reports:read"},
		},
		{
			name:         "a scope outside the client's own is rejected",
			clientScopes: []string{"sms:send"},
			requested:    "reports:read",
			wantErr:      true,
		},
		{
			name:         "one good scope alongside one bad one is still rejected",
			clientScopes: []string{"sms:send"},
			requested:    "sms:send reports:read",
			wantErr:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.GrantedScopes(tc.clientScopes, tc.requested)
			if tc.wantErr {
				if !errors.Is(err, domain.ErrScopeNotGranted) {
					t.Fatalf("got err %v, want ErrScopeNotGranted", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
