package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeRateLimiter is an in-memory RateLimiter for unit-testing the
// middleware's HTTP-layer behavior without Redis — RedisRateLimiter's
// own correctness is proven separately in
// ratelimit_integration_test.go. Mirrors hmacauth's fakeNonceStore
// pattern (platform/hmacauth/hmacauth_test.go): a small fake local to
// the test file, not a tools/fakes citizen — there's no adapter
// interface boundary here worth a shared fake.
type fakeRateLimiter struct {
	mu     sync.Mutex
	counts map[string]int
	deny   bool
}

func newFakeRateLimiter() *fakeRateLimiter {
	return &fakeRateLimiter{counts: make(map[string]int)}
}

func (f *fakeRateLimiter) Allow(_ context.Context, key string, limit int, _ time.Duration) (bool, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[key]++
	if f.deny || f.counts[key] > limit {
		return false, 30 * time.Second, nil
	}
	return true, 0, nil
}

func TestRateLimit_AllowsUnderLimit(t *testing.T) {
	limiter := newFakeRateLimiter()
	mw := rateLimit(limiter, rateLimitByIP, 2, time.Minute)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got status %d, want 200", i, rec.Code)
		}
	}
}

func TestRateLimit_RejectsOverLimitWithRetryAfter(t *testing.T) {
	limiter := newFakeRateLimiter()
	mw := rateLimit(limiter, rateLimitByIP, 1, time.Minute)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.1:1234"
		return req
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: got status %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, newReq())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got status %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header")
	}
	var body struct {
		Code string `json:"code"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != "RATE-4290" {
		t.Fatalf("got code %q, want RATE-4290", body.Code)
	}
}

func TestRateLimit_DifferentIPsAreIndependent(t *testing.T) {
	limiter := newFakeRateLimiter()
	mw := rateLimit(limiter, rateLimitByIP, 1, time.Minute)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	for _, ip := range []string{"203.0.113.1:1", "203.0.113.2:1"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = ip
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ip %s: got status %d, want 200", ip, rec.Code)
		}
	}
}

func TestRateLimit_ByClientID_KeysOnClaims(t *testing.T) {
	limiter := newFakeRateLimiter()
	mw := rateLimit(limiter, rateLimitByClientID, 1, time.Minute)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(withClaims(req.Context(), Claims{ClientID: "client-1"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}

	// No Claims in context at all (e.g. a route that isn't protected)
	// lets the request through rather than failing closed.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200 (no claims means no key, so no limit applies)", rec2.Code)
	}
}
