package idempotency_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
)

// fakeStore is an in-memory Store for unit-testing the middleware's
// HTTP-layer behavior without a database. The Postgres-backed Store's own
// correctness (the real ON CONFLICT claim semantics, expiry) is proven
// separately in postgres_integration_test.go.
type fakeStore struct {
	mu      sync.Mutex
	records map[string]*idempotency.Record
}

func newFakeStore() *fakeStore {
	return &fakeStore{records: make(map[string]*idempotency.Record)}
}

func (f *fakeStore) storeKey(tenantID uuid.UUID, key string) string {
	return tenantID.String() + "/" + key
}

func (f *fakeStore) Claim(_ context.Context, tenantID uuid.UUID, key string, requestHash []byte, _ time.Time) (*idempotency.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sk := f.storeKey(tenantID, key)
	if existing, ok := f.records[sk]; ok {
		snapshot := *existing
		return &snapshot, nil
	}
	f.records[sk] = &idempotency.Record{RequestHash: requestHash, Complete: false}
	return nil, nil
}

func (f *fakeStore) Complete(_ context.Context, tenantID uuid.UUID, key string, statusCode int, response []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sk := f.storeKey(tenantID, key)
	rec, ok := f.records[sk]
	if !ok {
		return nil
	}
	rec.StatusCode = statusCode
	rec.Response = response
	rec.Complete = true
	return nil
}

func withTenant(r *http.Request) *http.Request {
	ctx := vaslog.WithTenantID(r.Context(), uuid.New().String())
	return r.WithContext(ctx)
}

func TestMiddleware_MissingHeaderIsRejected(t *testing.T) {
	var handlerCalled bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	r := withTenant(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body")))
	w := httptest.NewRecorder()

	idempotency.Middleware(newFakeStore(), time.Hour)(next).ServeHTTP(w, r)

	if handlerCalled {
		t.Fatal("expected the handler not to run without an Idempotency-Key header")
	}
	if w.Code == http.StatusOK {
		t.Fatalf("expected a non-200 rejection, got %d", w.Code)
	}
}

func TestMiddleware_SameKeySameBody_ReturnsFirstResponseWithoutRerunning(t *testing.T) {
	var calls int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"first"}`))
	})

	store := newFakeStore()
	handler := idempotency.Middleware(store, time.Hour)(next)
	tenantCtx := vaslog.WithTenantID(context.Background(), uuid.New().String())

	newReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("same body"))
		r.Header.Set("Idempotency-Key", "key-1")
		return r.WithContext(tenantCtx)
	}

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, newReq())
	if calls != 1 {
		t.Fatalf("expected 1 call after the first request, got %d", calls)
	}
	if w1.Code != http.StatusCreated || w1.Body.String() != `{"id":"first"}` {
		t.Fatalf("unexpected first response: %d %q", w1.Code, w1.Body.String())
	}

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, newReq())
	if calls != 1 {
		t.Fatalf("expected the handler not to run again on replay, got %d calls", calls)
	}
	if w2.Code != http.StatusCreated || w2.Body.String() != `{"id":"first"}` {
		t.Fatalf("expected the replay to return the first response, got: %d %q", w2.Code, w2.Body.String())
	}
}

func TestMiddleware_SameKeyDifferentBody_ReturnsVAL4001(t *testing.T) {
	var calls int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})

	store := newFakeStore()
	handler := idempotency.Middleware(store, time.Hour)(next)
	tenantCtx := vaslog.WithTenantID(context.Background(), uuid.New().String())

	r1 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body A"))
	r1.Header.Set("Idempotency-Key", "key-1")
	handler.ServeHTTP(httptest.NewRecorder(), r1.WithContext(tenantCtx))
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}

	r2 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body B (different)"))
	r2.Header.Set("Idempotency-Key", "key-1")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, r2.WithContext(tenantCtx))

	if calls != 1 {
		t.Fatalf("expected the handler not to run for the conflicting body, got %d calls", calls)
	}

	var body map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["code"] != "VAL-4001" {
		t.Fatalf("code = %v, want VAL-4001", body["code"])
	}
}
