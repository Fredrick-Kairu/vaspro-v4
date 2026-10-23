package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

const testIssuer = "vaspro-identity"

// newTestJWKSServer serves a JWKS containing pub under kid — standing
// in for identity's real /.well-known/jwks.json (task 1.8), whose
// shape this mirrors exactly (a jwk.Set, Ed25519/OKP).
func newTestJWKSServer(t *testing.T, kid string, pub ed25519.PublicKey) *httptest.Server {
	t.Helper()
	key, err := jwk.Import(pub)
	if err != nil {
		t.Fatalf("importing public key: %v", err)
	}
	if err := key.Set(jwk.KeyIDKey, kid); err != nil {
		t.Fatalf("setting kid: %v", err)
	}
	if err := key.Set(jwk.AlgorithmKey, jwa.EdDSA()); err != nil {
		t.Fatalf("setting alg: %v", err)
	}
	if err := key.Set(jwk.KeyUsageKey, "sig"); err != nil {
		t.Fatalf("setting use: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(key); err != nil {
		t.Fatalf("adding key to set: %v", err)
	}
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling jwks: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type testTokenOpts struct {
	kid         string
	issuer      string
	expiresIn   time.Duration
	clientID    string
	tenantID    string
	environment string
	scope       string
	ipAllowlist string
}

func signTestToken(t *testing.T, priv ed25519.PrivateKey, opts testTokenOpts) string {
	t.Helper()
	key, err := jwk.Import(priv)
	if err != nil {
		t.Fatalf("importing private key: %v", err)
	}
	if err := key.Set(jwk.KeyIDKey, opts.kid); err != nil {
		t.Fatalf("setting kid: %v", err)
	}

	now := time.Now()
	builder := jwt.NewBuilder().
		Issuer(opts.issuer).
		Subject(opts.clientID).
		IssuedAt(now).
		Expiration(now.Add(opts.expiresIn)).
		Claim("client_id", opts.clientID).
		Claim("tid", opts.tenantID).
		Claim("env", opts.environment).
		Claim("scope", opts.scope).
		Claim("ip_allowlist", opts.ipAllowlist)

	token, err := builder.Build()
	if err != nil {
		t.Fatalf("building token: %v", err)
	}
	signed, err := jwt.Sign(token, jwt.WithKey(jwa.EdDSA(), key))
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return string(signed)
}

func defaultTokenOpts(kid string) testTokenOpts {
	return testTokenOpts{
		kid:         kid,
		issuer:      testIssuer,
		expiresIn:   time.Hour,
		clientID:    "client-1",
		tenantID:    "tenant-1",
		environment: "sandbox",
		scope:       "sms:send reports:read",
		ipAllowlist: "203.0.113.0/24",
	}
}

func TestRequireJWT_ValidToken_SetsClaims(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	srv := newTestJWKSServer(t, "kid-1", pub)

	var gotClaims Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClaims, _ = claimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	jwks := newJWKSCache(srv.URL, testkit.NewClock(time.Now()))
	handler := requireJWT(jwks, testkit.NewClock(time.Now()), testIssuer)(next)

	token := signTestToken(t, priv, defaultTokenOpts("kid-1"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	if gotClaims.ClientID != "client-1" {
		t.Fatalf("got client_id %q, want client-1", gotClaims.ClientID)
	}
	if gotClaims.TenantID != "tenant-1" {
		t.Fatalf("got tid %q, want tenant-1", gotClaims.TenantID)
	}
	if gotClaims.Environment != "sandbox" {
		t.Fatalf("got env %q, want sandbox", gotClaims.Environment)
	}
	if len(gotClaims.Scopes) != 2 || gotClaims.Scopes[0] != "sms:send" {
		t.Fatalf("got scopes %v, want [sms:send reports:read]", gotClaims.Scopes)
	}
	if len(gotClaims.IPAllowlist) != 1 || gotClaims.IPAllowlist[0] != "203.0.113.0/24" {
		t.Fatalf("got ip_allowlist %v, want [203.0.113.0/24]", gotClaims.IPAllowlist)
	}
}

func TestRequireJWT_Rejections(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating other key: %v", err)
	}
	srv := newTestJWKSServer(t, "kid-1", pub)
	clk := testkit.NewClock(time.Now())

	tests := []struct {
		name   string
		header string
		token  func() string
	}{
		{"no Authorization header", "", nil},
		{"malformed Authorization header", "not-a-bearer-token", nil},
		{"expired token", "", func() string {
			opts := defaultTokenOpts("kid-1")
			opts.expiresIn = -time.Hour
			return signTestToken(t, priv, opts)
		}},
		{"wrong issuer", "", func() string {
			opts := defaultTokenOpts("kid-1")
			opts.issuer = "someone-else"
			return signTestToken(t, priv, opts)
		}},
		{"unknown kid", "", func() string {
			return signTestToken(t, priv, defaultTokenOpts("kid-does-not-exist"))
		}},
		{"signed by a key not in the jwks", "", func() string {
			// Same kid the JWKS publishes, but signed with a different
			// private key — proves verification checks the actual
			// signature, not just that a kid happens to match.
			return signTestToken(t, otherPriv, defaultTokenOpts("kid-1"))
		}},
		{"garbage token", "garbage", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jwks := newJWKSCache(srv.URL, clk)
			handler := requireJWT(jwks, clk, testIssuer)(okHandler())

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			switch {
			case tc.token != nil:
				req.Header.Set("Authorization", "Bearer "+tc.token())
			case tc.header != "":
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got status %d, want 401", rec.Code)
			}
			var body struct {
				Code string `json:"code"`
			}
			decodeJSON(t, rec, &body)
			if body.Code != "AUTH-4010" {
				t.Fatalf("got code %q, want AUTH-4010", body.Code)
			}
		})
	}
}
