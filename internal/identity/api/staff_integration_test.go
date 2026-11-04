//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantaccess"
)

func TestCreateStaffUnlock(t *testing.T) {
	r, sender, pool := newTestRouterWithPool(t, nil, &tenantaccess.Fake{AllowAll: true})
	signUpVerifyAndSetPassword(t, r, sender, "staff@example.com", "a-strong-password")
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")
	signUpVerifyAndSetPassword(t, r, sender, "locked@example.com", "a-strong-password")
	_, staffToken := signInForTokens(t, r, "staff@example.com", "a-strong-password", "staff-signin")
	_, adaToken := signInForTokens(t, r, "ada@example.com", "a-strong-password", "ada-signin")
	// The ops CLI's path: the same app use case on the same database.
	ops := app.New(app.Deps{Pool: pool, Clock: clock.Real{}})
	if err := ops.GrantStaff(t.Context(), "staff@example.com", "ops"); err != nil {
		t.Fatal(err)
	}
	attempt := 0
	signInWith := func(password string) int {
		attempt++
		status, _ := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
			map[string]string{"email": "locked@example.com", "password": password}, fmt.Sprintf("locked-signin-%d", attempt))
		return status
	}
	signIn := func() int { return signInWith("a-strong-password") }
	for i := 0; i < 5; i++ {
		if status := signInWith("wrong"); status != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: got %d", i, status)
		}
	}
	if status := signIn(); status != http.StatusUnauthorized {
		t.Fatalf("expected the lockout, got %d", status)
	}

	cases := []struct {
		name, token, email string
		wantStatus         int
		wantCode           string
	}{
		{"no session", "", "locked@example.com", http.StatusUnauthorized, "AUTH-4010"},
		{"not staff", adaToken, "locked@example.com", http.StatusForbidden, "AUTH-4025"},
		{"unknown email", staffToken, "nobody@example.com", http.StatusNotFound, "VAL-4003"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doJSON(t, r, http.MethodPost, "/v1/auth/staff/unlocks", tc.token,
				map[string]string{"email": tc.email}, "unlock-"+tc.name)
			if status != tc.wantStatus || body["code"] != tc.wantCode {
				t.Fatalf("got %d %v, want %d %s", status, body, tc.wantStatus, tc.wantCode)
			}
		})
	}
	if status := signIn(); status != http.StatusUnauthorized {
		t.Fatalf("refusals unlocked the user: %d", status)
	}

	status, body := doJSON(t, r, http.MethodPost, "/v1/auth/staff/unlocks", staffToken,
		map[string]string{"email": "locked@example.com"}, "unlock-ok")
	if status != http.StatusNoContent {
		t.Fatalf("staff unlock: got %d %v", status, body)
	}
	if status := signIn(); status != http.StatusOK {
		t.Fatalf("after the unlock: got %d", status)
	}
}
