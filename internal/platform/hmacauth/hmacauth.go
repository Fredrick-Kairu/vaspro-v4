// Package hmacauth verifies the HMAC-SHA256 signature system design §3
// requires on high-value calls (payouts, B2B, wallet transfers): a
// signature over method, path, body and timestamp, valid for 5
// minutes, with replay protection. It's a second factor on top of the
// JWT a client already obtained via OAuth2 (task 1.8) — the gateway
// checks the JWT first (task 1.11), then this for the subset of calls
// system design calls high-value.
//
// This package is deliberately generic and identity-agnostic: no
// "high-value" endpoint exists anywhere in this codebase yet
// (payouts/B2B/wallet transfers are payments/ledger concepts, Phase
// 3+), so there's no api_clients-style table to look a signing key up
// in yet either. Middleware takes the signing key via a caller-supplied
// KeyFunc instead of querying anything itself — whichever future task
// builds the first real high-value endpoint decides where that
// function's answer comes from.
package hmacauth

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// Header names. No existing convention in this repo beyond the bare
// Idempotency-Key, so these follow common signed-webhook practice.
const (
	HeaderSignature = "X-Signature"
	HeaderTimestamp = "X-Signature-Timestamp"
	HeaderNonce     = "X-Signature-Nonce"
)

// KeyFunc resolves clientID's raw HMAC signing key. Middleware never
// looks this up itself — see the package doc for why.
type KeyFunc func(ctx context.Context, clientID string) ([]byte, error)

// NonceStore gives each nonce exactly one successful Claim — the
// mechanism behind replay protection. Claim returns false if nonce was
// already claimed (a replay), true if this call is the one that
// claimed it.
type NonceStore interface {
	Claim(ctx context.Context, nonce string, ttl time.Duration) (claimed bool, err error)
}

type clientIDKey struct{}

// WithClientID stands in for whatever the gateway's JWT-auth
// middleware will eventually put in context once it exists (task
// 1.11) — this check runs after JWT auth in the real pipeline, so the
// caller is already known by the time it runs; it's never read from a
// request header here.
func WithClientID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, clientIDKey{}, id)
}

// ClientID returns the client ID WithClientID stored, if any.
func ClientID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(clientIDKey{}).(string)
	return id, ok
}

// CanonicalString builds the exact bytes a client signs and this
// package verifies: method, path, timestamp and body, newline-joined.
// path must not include a query string. timestamp is the raw header
// value as sent, not reformatted — signer and verifier must agree on
// its exact bytes.
func CanonicalString(method, path, timestamp string, body []byte) []byte {
	buf := make([]byte, 0, len(method)+len(path)+len(timestamp)+len(body)+3)
	buf = append(buf, method...)
	buf = append(buf, '\n')
	buf = append(buf, path...)
	buf = append(buf, '\n')
	buf = append(buf, timestamp...)
	buf = append(buf, '\n')
	buf = append(buf, body...)
	return buf
}

// Middleware verifies HeaderSignature/HeaderTimestamp/HeaderNonce on
// every request it wraps. maxAge is both the timestamp validity
// window and the nonce's remembered lifetime — system design §3 names
// 5 minutes. Every rejection renders the same AUTH-4013
// (deliberately not distinguished, same anti-enumeration principle
// every other auth-adjacent code in this catalogue already uses) —
// except a missing ClientID in context, which means this middleware
// was wired in before JWT auth ran: a wiring bug, not a client error,
// so it falls through to the generic SYS-9000 path instead.
//
// Reads and restores the request body (platform/idempotency.Middleware's
// own pattern, for the same reason: oapi-codegen's strict-server
// decodes the body into a typed struct before any StrictMiddlewareFunc
// runs, so by then it's too late to hash the exact bytes a client
// signed — this has to run as a plain net/http middleware, earlier in
// the chain).
func Middleware(keyFunc KeyFunc, nonces NonceStore, maxAge time.Duration, clk clock.Clock) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientID, ok := ClientID(r.Context())
			if !ok {
				problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
				return
			}

			sig := r.Header.Get(HeaderSignature)
			ts := r.Header.Get(HeaderTimestamp)
			nonce := r.Header.Get(HeaderNonce)
			if sig == "" || ts == "" || nonce == "" {
				reject(w, r)
				return
			}

			tsSeconds, err := strconv.ParseInt(ts, 10, 64)
			if err != nil {
				reject(w, r)
				return
			}
			age := clk.Now().UTC().Sub(time.Unix(tsSeconds, 0).UTC())
			if age < 0 {
				age = -age
			}
			if age > maxAge {
				reject(w, r)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				reject(w, r)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			key, err := keyFunc(r.Context(), clientID)
			if err != nil {
				problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
				return
			}

			wantSig, err := hex.DecodeString(sig)
			if err != nil {
				reject(w, r)
				return
			}
			canonical := CanonicalString(r.Method, r.URL.Path, ts, body)
			if !vascrypto.HMACVerify(key, canonical, wantSig) {
				reject(w, r)
				return
			}

			// Claimed only after a verified signature: a wrong signature
			// never burns a nonce a legitimate retry might need, and
			// claiming here is what actually catches a replay of a
			// previously-valid request.
			claimed, err := nonces.Claim(r.Context(), nonce, maxAge)
			if err != nil {
				problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
				return
			}
			if !claimed {
				reject(w, r)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func reject(w http.ResponseWriter, r *http.Request) {
	problem.Respond(w, r, problem.New(problem.AUTH4013, "the request's signature is invalid or has expired"))
}
