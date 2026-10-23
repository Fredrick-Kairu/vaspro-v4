package hmacauth_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/hmacauth"
)

const (
	testClientID = "client-1"
	testMaxAge   = 5 * time.Minute
)

var testKey = []byte("a test signing key, not a real secret")

// fakeNonceStore is an in-memory NonceStore for unit-testing the
// middleware's HTTP-layer behavior without Redis. RedisNonceStore's
// own correctness (the real atomic SETNX claim) is proven separately
// in redisnoncestore_integration_test.go.
type fakeNonceStore struct {
	mu      sync.Mutex
	claimed map[string]bool
}

func newFakeNonceStore() *fakeNonceStore {
	return &fakeNonceStore{claimed: make(map[string]bool)}
}

func (f *fakeNonceStore) Claim(_ context.Context, nonce string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed[nonce] {
		return false, nil
	}
	f.claimed[nonce] = true
	return true, nil
}

var errUnknownClient = errors.New("hmacauth_test: unknown client")

func testKeyFunc(_ context.Context, clientID string) ([]byte, error) {
	if clientID != testClientID {
		return nil, errUnknownClient // unreachable in these tests; every one uses testClientID
	}
	return testKey, nil
}

// signedRequest builds an httptest.Request for method/path/body, signed
// with key under timestamp ts and nonce, with clientID already in
// context (standing in for the gateway's JWT-auth middleware, task
// 1.11, which doesn't exist yet — see the package doc).
func signedRequest(t *testing.T, method, path string, body []byte, key []byte, ts time.Time, nonce string) *http.Request {
	t.Helper()
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	sig := vascrypto.HMACSign(key, hmacauth.CanonicalString(method, path, tsStr, body))

	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set(hmacauth.HeaderSignature, hex.EncodeToString(sig))
	r.Header.Set(hmacauth.HeaderTimestamp, tsStr)
	r.Header.Set(hmacauth.HeaderNonce, nonce)
	return r.WithContext(hmacauth.WithClientID(r.Context(), testClientID))
}

func decodeProblem(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding problem body %q: %v", w.Body.String(), err)
	}
	return decoded
}

func echoHandler(t *testing.T) (http.Handler, *[]byte) {
	t.Helper()
	var seenBody []byte
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("handler reading body: %v", err)
		}
		seenBody = b
		w.WriteHeader(http.StatusOK)
	})
	return h, &seenBody
}

func TestMiddleware_ValidSignature_PassesThroughAndPreservesBody(t *testing.T) {
	clk := clock.NewFake(time.Now().UTC())
	next, seenBody := echoHandler(t)

	body := []byte(`{"amount_minor":100000}`)
	r := signedRequest(t, http.MethodPost, "/v1/payouts", body, testKey, clk.Now(), "nonce-1")
	w := httptest.NewRecorder()

	hmacauth.Middleware(testKeyFunc, newFakeNonceStore(), testMaxAge, clk)(next).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if string(*seenBody) != string(body) {
		t.Fatalf("handler saw body %q, want %q (middleware must restore the body it read)", *seenBody, body)
	}
}

func TestMiddleware_WrongSignature_Rejected(t *testing.T) {
	clk := clock.NewFake(time.Now().UTC())
	next, _ := echoHandler(t)

	r := signedRequest(t, http.MethodPost, "/v1/payouts", []byte("body"), testKey, clk.Now(), "nonce-1")
	r.Header.Set(hmacauth.HeaderSignature, hex.EncodeToString(vascrypto.HMACSign([]byte("wrong key"), []byte("whatever"))))
	w := httptest.NewRecorder()

	hmacauth.Middleware(testKeyFunc, newFakeNonceStore(), testMaxAge, clk)(next).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", w.Code)
	}
	if got := decodeProblem(t, w)["code"]; got != "AUTH-4013" {
		t.Fatalf("got code %v, want AUTH-4013", got)
	}
}

func TestMiddleware_TamperedBody_Rejected(t *testing.T) {
	// Same signature, different body than what was signed — proves
	// the body is actually part of what's verified, not just present.
	clk := clock.NewFake(time.Now().UTC())
	next, _ := echoHandler(t)

	r := signedRequest(t, http.MethodPost, "/v1/payouts", []byte("original body"), testKey, clk.Now(), "nonce-1")
	r.Body = io.NopCloser(bytes.NewReader([]byte("tampered body")))
	w := httptest.NewRecorder()

	hmacauth.Middleware(testKeyFunc, newFakeNonceStore(), testMaxAge, clk)(next).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", w.Code)
	}
}

func TestMiddleware_StaleTimestamp_Rejected(t *testing.T) {
	clk := clock.NewFake(time.Now().UTC())
	next, _ := echoHandler(t)

	old := clk.Now().Add(-testMaxAge - time.Second)
	r := signedRequest(t, http.MethodPost, "/v1/payouts", []byte("body"), testKey, old, "nonce-1")
	w := httptest.NewRecorder()

	hmacauth.Middleware(testKeyFunc, newFakeNonceStore(), testMaxAge, clk)(next).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", w.Code)
	}
	if got := decodeProblem(t, w)["code"]; got != "AUTH-4013" {
		t.Fatalf("got code %v, want AUTH-4013", got)
	}
}

func TestMiddleware_FutureTimestamp_Rejected(t *testing.T) {
	clk := clock.NewFake(time.Now().UTC())
	next, _ := echoHandler(t)

	future := clk.Now().Add(testMaxAge + time.Second)
	r := signedRequest(t, http.MethodPost, "/v1/payouts", []byte("body"), testKey, future, "nonce-1")
	w := httptest.NewRecorder()

	hmacauth.Middleware(testKeyFunc, newFakeNonceStore(), testMaxAge, clk)(next).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 (a timestamp far in the future should be rejected too, not just a stale one)", w.Code)
	}
}

func TestMiddleware_ReplayedNonce_SecondRequestRejected(t *testing.T) {
	clk := clock.NewFake(time.Now().UTC())
	next, _ := echoHandler(t)
	nonces := newFakeNonceStore()

	r1 := signedRequest(t, http.MethodPost, "/v1/payouts", []byte("body"), testKey, clk.Now(), "shared-nonce")
	w1 := httptest.NewRecorder()
	hmacauth.Middleware(testKeyFunc, nonces, testMaxAge, clk)(next).ServeHTTP(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: got status %d, want 200 (body: %s)", w1.Code, w1.Body.String())
	}

	r2 := signedRequest(t, http.MethodPost, "/v1/payouts", []byte("body"), testKey, clk.Now(), "shared-nonce")
	w2 := httptest.NewRecorder()
	hmacauth.Middleware(testKeyFunc, nonces, testMaxAge, clk)(next).ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("replayed request: got status %d, want 401", w2.Code)
	}
	if got := decodeProblem(t, w2)["code"]; got != "AUTH-4013" {
		t.Fatalf("got code %v, want AUTH-4013", got)
	}
}

func TestMiddleware_MissingHeader_Rejected(t *testing.T) {
	cases := []string{hmacauth.HeaderSignature, hmacauth.HeaderTimestamp, hmacauth.HeaderNonce}
	for _, missing := range cases {
		t.Run(missing, func(t *testing.T) {
			clk := clock.NewFake(time.Now().UTC())
			next, _ := echoHandler(t)

			r := signedRequest(t, http.MethodPost, "/v1/payouts", []byte("body"), testKey, clk.Now(), "nonce-1")
			r.Header.Del(missing)
			w := httptest.NewRecorder()

			hmacauth.Middleware(testKeyFunc, newFakeNonceStore(), testMaxAge, clk)(next).ServeHTTP(w, r)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("got status %d, want 401", w.Code)
			}
		})
	}
}

func TestMiddleware_NoClientIDInContext_IsAWiringBugNotAClientError(t *testing.T) {
	clk := clock.NewFake(time.Now().UTC())
	next, _ := echoHandler(t)

	// Deliberately not using signedRequest: no WithClientID at all,
	// standing in for this middleware being wired in before JWT auth
	// ran (task 1.11) — a wiring bug, not something a client did wrong.
	r := httptest.NewRequest(http.MethodPost, "/v1/payouts", bytes.NewReader([]byte("body")))
	w := httptest.NewRecorder()

	hmacauth.Middleware(testKeyFunc, newFakeNonceStore(), testMaxAge, clk)(next).ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", w.Code)
	}
	if got := decodeProblem(t, w)["code"]; got != "SYS-9000" {
		t.Fatalf("got code %v, want SYS-9000 (not AUTH-4013 — this isn't the client's fault)", got)
	}
}
