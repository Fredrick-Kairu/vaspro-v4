// Package oidc fakes an OpenID Connect provider (Google, task 1.16)
// for tests: a real HTTP server with a discovery document, a JWKS and a
// token endpoint, so identity's real go-oidc adapter runs unchanged
// against it. A test plays the user's browser with Authorize, which
// reads the authorization URL identity built and returns the code the
// provider would redirect back with.
package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

const keyID = "fake-oidc-key"

// User is who signs in at the fake provider.
type User struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	HostedDomain  string
}

// Tamper changes the ID token the next code yields, to test each
// verification failure. The zero value issues a correct token.
type Tamper struct {
	Nonce           string        // overrides the nonce from the authorization URL
	Audience        string        // overrides the client ID
	Issuer          string        // overrides the provider's own issuer
	ExpiresIn       time.Duration // default one hour; negative = already expired
	BadSignature    bool          // flips a byte of the signature
	WrongSigningKey bool          // signs with a key not in the JWKS
}

type pending struct {
	user          User
	nonce         string
	codeChallenge string
	tamper        Tamper
}

// Provider is a running fake OIDC provider.
type Provider struct {
	ClientID string

	server   *httptest.Server
	key      *rsa.PrivateKey
	otherKey *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]pending
	// IDTokens records every ID token issued, so a test can grep logs
	// for them.
	IDTokens []string
}

// New starts a fake provider for clientID. Close it when done.
func New(clientID string) (*Provider, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	p := &Provider{ClientID: clientID, key: key, otherKey: otherKey, codes: map[string]pending{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("POST /token", p.token)
	p.server = httptest.NewServer(mux)
	return p, nil
}

// Issuer is the provider's issuer URL (pass it to identity's config).
func (p *Provider) Issuer() string { return p.server.URL }

// Close stops the server.
func (p *Provider) Close() { p.server.Close() }

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                p.Issuer(),
		"authorization_endpoint":                p.Issuer() + "/authorize",
		"token_endpoint":                        p.Issuer() + "/token",
		"jwks_uri":                              p.Issuer() + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	pub, err := jwk.Import(p.key.Public())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = pub.Set(jwk.KeyIDKey, keyID)
	_ = pub.Set(jwk.AlgorithmKey, jwa.RS256())
	set := jwk.NewSet()
	_ = set.AddKey(pub)
	writeJSON(w, set)
}

// Authorize plays the browser: it reads the authorization URL identity
// returned, "signs in" user and returns the code the provider would
// redirect back with, together with the state from the URL.
func (p *Provider) Authorize(authorizationURL string, user User, tamper Tamper) (code, state string, err error) {
	u, err := url.Parse(authorizationURL)
	if err != nil {
		return "", "", err
	}
	q := u.Query()
	if q.Get("client_id") != p.ClientID {
		return "", "", fmt.Errorf("fake oidc: client_id %q, want %q", q.Get("client_id"), p.ClientID)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		return "", "", errors.New("fake oidc: authorization URL has no S256 PKCE challenge")
	}
	if q.Get("nonce") == "" || q.Get("state") == "" {
		return "", "", errors.New("fake oidc: authorization URL has no nonce or state")
	}
	code = rand.Text()
	p.mu.Lock()
	p.codes[code] = pending{user: user, nonce: q.Get("nonce"), codeChallenge: q.Get("code_challenge"), tamper: tamper}
	p.mu.Unlock()
	return code, q.Get("state"), nil
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	pend, ok := p.codes[code]
	delete(p.codes, code) // codes are single use
	p.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != pend.codeChallenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant", "error_description": "PKCE verifier mismatch"})
		return
	}
	idToken, err := p.sign(pend)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.mu.Lock()
	p.IDTokens = append(p.IDTokens, idToken)
	p.mu.Unlock()
	writeJSON(w, map[string]any{
		"access_token": rand.Text(),
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func (p *Provider) sign(pend pending) (string, error) {
	t := pend.tamper
	nonce, aud, iss := pend.nonce, p.ClientID, p.Issuer()
	if t.Nonce != "" {
		nonce = t.Nonce
	}
	if t.Audience != "" {
		aud = t.Audience
	}
	if t.Issuer != "" {
		iss = t.Issuer
	}
	expiresIn := time.Hour
	if t.ExpiresIn != 0 {
		expiresIn = t.ExpiresIn
	}
	now := time.Now()
	b := jwt.NewBuilder().
		Issuer(iss).
		Subject(pend.user.Subject).
		Audience([]string{aud}).
		IssuedAt(now.Add(-time.Minute)).
		Expiration(now.Add(expiresIn)).
		Claim("nonce", nonce).
		Claim("email", pend.user.Email).
		Claim("email_verified", pend.user.EmailVerified).
		Claim("name", pend.user.Name)
	if pend.user.HostedDomain != "" {
		b = b.Claim("hd", pend.user.HostedDomain)
	}
	tok, err := b.Build()
	if err != nil {
		return "", err
	}
	raw := p.key
	if t.WrongSigningKey {
		raw = p.otherKey
	}
	key, err := jwk.Import(raw)
	if err != nil {
		return "", err
	}
	if err := key.Set(jwk.KeyIDKey, keyID); err != nil {
		return "", err
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), key))
	if err != nil {
		return "", err
	}
	if t.BadSignature {
		signed[len(signed)-5] ^= 0x01
		if signed[len(signed)-5] == '.' || signed[len(signed)-5] > 'z' {
			signed[len(signed)-5] = 'A'
		}
	}
	return string(signed), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
