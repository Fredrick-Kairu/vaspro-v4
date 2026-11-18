//go:build integration

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantaccess"
)

// TestOneTimeCredentials_NotKeptForReplays (task 5.5): every response
// that shows a credential once (an API client's secret on create and
// rotate, an SMPP password on create and reset, a TOTP enrolment's
// otpauth URL and recovery codes) is stored for Idempotency-Key replays
// with that credential null. A retry with the same key does nothing
// twice and shows nothing again, and idempotency_keys never holds the
// credential.
func TestOneTimeCredentials_NotKeptForReplays(t *testing.T) {
	r, sender, pool := newTestRouterWithPool(t, nil, &tenantaccess.Fake{AllowAll: true})
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")
	_, signInBody := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "ada@example.com", "password": "a-strong-password"}, "signin-key")
	session, _ := signInBody["session_token"].(string)
	selectTenant(t, r, session)
	stepUpViaEmail(t, r, sender, session)

	// twice sends the same request with the same key, checks both
	// answers have status want, and returns them.
	twice := func(t *testing.T, path string, body any, key string, want int) (first, second map[string]any) {
		t.Helper()
		status, first := doJSON(t, r, http.MethodPost, path, session, body, key)
		if status != want {
			t.Fatalf("%s: got %d %v", path, status, first)
		}
		status, second = doJSON(t, r, http.MethodPost, path, session, body, key)
		if status != want {
			t.Fatalf("%s replayed: got %d %v", path, status, second)
		}
		return first, second
	}
	var shown []string

	t.Run("an API client's secret", func(t *testing.T) {
		first, again := twice(t, "/v1/auth/api-clients",
			map[string]any{"name": "Reporting", "environment": "sandbox", "scopes": []string{"reports:read"}}, "client-1", http.StatusOK)
		secret := first["secret"].(map[string]any)
		shown = append(shown, secret["secret"].(string))
		replayed := again["secret"].(map[string]any)
		if again["id"] != first["id"] || replayed["secret"] != nil || replayed["id"] != secret["id"] || replayed["last4"] != secret["last4"] {
			t.Fatalf("replay %v, want the same client with secret.secret null", again)
		}
		id := first["id"].(string)
		rotated, rotatedAgain := twice(t, "/v1/auth/api-clients/"+id+"/secrets", nil, "rotate-1", http.StatusOK)
		shown = append(shown, rotated["secret"].(string))
		if rotatedAgain["secret"] != nil || rotatedAgain["id"] != rotated["id"] {
			t.Fatalf("rotation replay %v", rotatedAgain)
		}
		_, list := doJSON(t, r, http.MethodGet, "/v1/auth/api-clients", session, nil, "")
		if clients := list["clients"].([]any); len(clients) != 1 {
			t.Fatalf("%d clients after a replayed create", len(clients))
		}
	})

	t.Run("an SMPP password", func(t *testing.T) {
		first, again := twice(t, "/v1/auth/smpp-accounts", map[string]any{
			"system_id": "ACME01", "environment": "sandbox", "ip_allowlist": []string{"203.0.113.0/24"}, "max_binds": 2, "max_tps": 50,
		}, "smpp-1", http.StatusOK)
		shown = append(shown, first["password"].(string))
		if again["id"] != first["id"] || again["password"] != nil {
			t.Fatalf("replay %v", again)
		}
		reset, resetAgain := twice(t, "/v1/auth/smpp-accounts/"+first["id"].(string)+"/password", nil, "smpp-reset-1", http.StatusOK)
		shown = append(shown, reset["password"].(string))
		if resetAgain["password"] != nil {
			t.Fatalf("reset replay %v", resetAgain)
		}
	})

	t.Run("a TOTP enrolment", func(t *testing.T) {
		first, again := twice(t, "/v1/auth/totp-enrollments", nil, "totp-1", http.StatusOK)
		url := first["url"].(string)
		shown = append(shown, url[strings.Index(url, "secret="):]) // the TOTP secret is in the URL
		for _, c := range first["recovery_codes"].([]any) {
			shown = append(shown, c.(string))
		}
		if again["url"] != nil || again["recovery_codes"] != nil {
			t.Fatalf("replay %v", again)
		}
	})

	t.Run("idempotency_keys holds none of them", func(t *testing.T) {
		rows, err := pool.Query(context.Background(), `SELECT response::text FROM idempotency_keys WHERE response IS NOT NULL`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var resp string
			if err := rows.Scan(&resp); err != nil {
				t.Fatal(err)
			}
			n++
			for _, s := range shown {
				if strings.Contains(resp, s) {
					t.Fatalf("a stored response holds a one-time credential")
				}
			}
		}
		if n == 0 || len(shown) < 5 {
			t.Fatalf("checked %d stored responses for %d credentials; the test isn't looking", n, len(shown))
		}
	})
}
