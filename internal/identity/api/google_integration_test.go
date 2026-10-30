//go:build integration

package api_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity"
	fakeoidc "github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/oidc"
)

const testGoogleClientID = "vaspro-test-client"

func newGoogleTestRouter(t *testing.T) (http.Handler, *fakeoidc.Provider) {
	t.Helper()
	idp, err := fakeoidc.New(testGoogleClientID)
	if err != nil {
		t.Fatalf("starting fake OIDC provider: %v", err)
	}
	t.Cleanup(idp.Close)
	r, _ := newTestRouterWith(t, func(cfg *identity.Config) {
		cfg.GoogleClientID = testGoogleClientID
		cfg.GoogleClientSecret = "test-secret"
		cfg.GoogleRedirectURL = "https://localhost/auth/google/callback"
		cfg.GoogleIssuer = idp.Issuer()
	})
	return r, idp
}

func problemCode(body map[string]any) string {
	code, _ := body["code"].(string)
	return code
}

// googleCallback starts a flow over HTTP, signs user in at the fake
// provider and posts the callback, returning its status and body.
func googleCallback(t *testing.T, r http.Handler, idp *fakeoidc.Provider, user fakeoidc.User, tamper fakeoidc.Tamper, key string) (int, map[string]any) {
	t.Helper()
	status, auth := doJSON(t, r, http.MethodPost, "/v1/auth/google/authorizations", "", nil, "google-auth-"+key)
	if status != http.StatusOK {
		t.Fatalf("createGoogleAuthorization: got %d %v", status, auth)
	}
	code, state, err := idp.Authorize(auth["authorization_url"].(string), user, tamper)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return doJSON(t, r, http.MethodPost, "/v1/auth/google/sessions", "",
		map[string]string{"code": code, "state": state}, "google-session-"+key)
}

func TestGoogle_NotConfigured_Returns404(t *testing.T) {
	r, _ := newTestRouter(t)
	status, body := doJSON(t, r, http.MethodPost, "/v1/auth/google/authorizations", "", nil, "google-off")
	if status != http.StatusNotFound || problemCode(body) != "VAL-4003" {
		t.Fatalf("got %d %v, want 404 VAL-4003", status, body)
	}
}

func TestGoogle_SignUpEnrolTOTPAndSignInOverHTTP(t *testing.T) {
	r, idp := newGoogleTestRouter(t)
	user := fakeoidc.User{Subject: "sub-http", Email: "http@example.com", EmailVerified: true, Name: "Hedy Lamarr"}

	status, body := googleCallback(t, r, idp, user, fakeoidc.Tamper{}, "1")
	if status != http.StatusOK || body["enrollment_token"] == nil || body["session_token"] != nil {
		t.Fatalf("sign-up callback: got %d %v, want only an enrollment_token", status, body)
	}
	enrollment := body["enrollment_token"].(string)

	status, body = doJSON(t, r, http.MethodPost, "/v1/auth/google/enrollments/totp", "",
		map[string]string{"enrollment_token": enrollment}, "google-totp-1")
	if status != http.StatusOK {
		t.Fatalf("createGoogleTotpEnrollment: got %d %v", status, body)
	}
	secret := totpSecretFromOTPAuthURL(t, body["url"].(string))
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body = doJSON(t, r, http.MethodPost, "/v1/auth/google/enrollments/totp/confirmation", "",
		map[string]string{"enrollment_token": enrollment, "code": code}, "google-totp-confirm-1")
	if status != http.StatusOK || body["session_token"] == nil {
		t.Fatalf("createGoogleTotpEnrollmentConfirmation: got %d %v, want a session", status, body)
	}
	sessionToken := body["session_token"].(string)

	t.Run("signing in again returns a challenge", func(t *testing.T) {
		status, body := googleCallback(t, r, idp, user, fakeoidc.Tamper{}, "2")
		if status != http.StatusOK || body["challenge"] == nil || body["session_token"] != nil {
			t.Fatalf("got %d %v, want a challenge only", status, body)
		}
	})
	t.Run("a tampered ID token is AUTH-4018", func(t *testing.T) {
		status, body := googleCallback(t, r, idp, user, fakeoidc.Tamper{BadSignature: true}, "3")
		if status != http.StatusUnauthorized || problemCode(body) != "AUTH-4018" {
			t.Fatalf("got %d %v, want 401 AUTH-4018", status, body)
		}
	})
	t.Run("linking needs step-up, and Google sign-in is not step-up", func(t *testing.T) {
		status, body := doJSON(t, r, http.MethodPost, "/v1/auth/google/link-authorizations", sessionToken, nil, "google-link-1")
		if status != http.StatusUnauthorized || problemCode(body) != "AUTH-4012" {
			t.Fatalf("got %d %v, want 401 AUTH-4012", status, body)
		}
	})
	t.Run("the linked account is shown", func(t *testing.T) {
		status, body := doJSON(t, r, http.MethodGet, "/v1/auth/google/link", sessionToken, nil, "")
		if status != http.StatusOK || body["email"] != "http@example.com" || body["provider"] != "google" {
			t.Fatalf("got %d %v", status, body)
		}
	})
	t.Run("unlinking the only way in is AUTH-4022", func(t *testing.T) {
		status, body := doJSON(t, r, http.MethodDelete, "/v1/auth/google/link", sessionToken, nil, "google-unlink-1")
		if status != http.StatusConflict || problemCode(body) != "AUTH-4022" {
			t.Fatalf("got %d %v, want 409 AUTH-4022", status, body)
		}
	})
}

func TestGoogle_ExistingEmail_Returns409AUTH4020(t *testing.T) {
	r, idp := newGoogleTestRouter(t)
	status, body := doJSON(t, r, http.MethodPost, "/v1/auth/signups", "",
		map[string]string{"email": "taken@example.com", "full_name": "Taken"}, "signup-taken")
	if status != http.StatusAccepted {
		t.Fatalf("sign-up: got %d %v", status, body)
	}
	status, body = googleCallback(t, r, idp,
		fakeoidc.User{Subject: "sub-taken", Email: "taken@example.com", EmailVerified: true}, fakeoidc.Tamper{}, "taken")
	if status != http.StatusConflict || problemCode(body) != "AUTH-4020" {
		t.Fatalf("got %d %v, want 409 AUTH-4020", status, body)
	}
}

// totpSecretFromOTPAuthURL reads the base32 secret from the otpauth://
// URL an enrollment returns.
func totpSecretFromOTPAuthURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing otpauth URL: %v", err)
	}
	secret := u.Query().Get("secret")
	if secret == "" {
		t.Fatal("otpauth URL has no secret")
	}
	return secret
}
