package idempotency_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
)

// secretRoutes matches the requests whose responses carry a one-time
// secret in these tests.
func secretRoutes(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/secrets") }

// redactCase runs one request through the middleware with redaction on,
// then the same request again, returning the live and replayed
// responses and what the store kept.
func redactCase(t *testing.T, path string, status int, body string) (live, replay *httptest.ResponseRecorder, stored []byte) {
	t.Helper()
	calls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	store := newFakeStore()
	tenant := uuid.New()
	handler := idempotency.Middleware(store, time.Hour, idempotency.WithRedactedFields(secretRoutes, "secret", "password"))(next)
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"name":"x"}`))
		r.Header.Set(idempotency.HeaderName, "key-1")
		return r.WithContext(vaslog.WithTenantID(context.Background(), tenant.String()))
	}
	live = httptest.NewRecorder()
	handler.ServeHTTP(live, req())
	replay = httptest.NewRecorder()
	handler.ServeHTTP(replay, req())
	if calls != 1 {
		t.Fatalf("the handler ran %d times, want once", calls)
	}
	return live, replay, store.records[store.storeKey(tenant, "key-1")].Response
}

func TestMiddleware_RedactsOneTimeSecretsFromWhatItKeeps(t *testing.T) {
	const body = `{"id":"c1","secret":"sk_live_abc","password":"p4ss","name":"orders","nested":{"secret":"kept"}}`
	live, replay, stored := redactCase(t, "/secrets", http.StatusCreated, body)

	if live.Code != http.StatusCreated || live.Body.String() != body {
		t.Fatalf("the live response changed: %d %s", live.Code, live.Body)
	}
	if strings.Contains(string(stored), "sk_live_abc") || strings.Contains(string(stored), "p4ss") {
		t.Fatalf("the stored response keeps the secret: %s", stored)
	}
	var got map[string]any
	if err := json.Unmarshal(replay.Body.Bytes(), &got); err != nil {
		t.Fatalf("replay %q: %v", replay.Body, err)
	}
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay status %d", replay.Code)
	}
	if v, ok := got["secret"]; !ok || v != nil {
		t.Fatalf("replayed secret = %v (present %v), want null", v, ok)
	}
	if v, ok := got["password"]; !ok || v != nil {
		t.Fatalf("replayed password = %v (present %v), want null", v, ok)
	}
	if got["id"] != "c1" || got["name"] != "orders" || got["nested"].(map[string]any)["secret"] != "kept" {
		t.Fatalf("other fields changed: %v (only top-level fields are redacted)", got)
	}
}

func TestMiddleware_RedactionLeavesOtherResponsesAlone(t *testing.T) {
	cases := []struct {
		name, path string
		status     int
		body       string
	}{
		{"another route", "/clients", http.StatusCreated, `{"secret":"sk_live_abc"}`},
		{"an error answer", "/secrets", http.StatusBadRequest, `{"code":"VAL-4000","secret":"echoed"}`},
		{"not a JSON object", "/secrets", http.StatusOK, `["secret"]`},
		{"no such field", "/secrets", http.StatusOK, `{"id":"c1"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, replay, stored := redactCase(t, c.path, c.status, c.body)
			if string(stored) != c.body || replay.Body.String() != c.body || replay.Code != c.status {
				t.Fatalf("stored %s, replayed %d %s; want both unchanged", stored, replay.Code, replay.Body)
			}
		})
	}
}

func TestMiddleware_RedactsANestedField(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","secret":{"id":"s1","secret":"sk_live_abc","last4":"labc"}}`))
	})
	store := newFakeStore()
	tenant := uuid.New()
	handler := idempotency.Middleware(store, time.Hour, idempotency.WithRedactedFields(secretRoutes, "secret.secret", "missing.path"))(next)
	r := httptest.NewRequest(http.MethodPost, "/secrets", strings.NewReader(`{}`))
	r.Header.Set(idempotency.HeaderName, "key-1")
	handler.ServeHTTP(httptest.NewRecorder(), r.WithContext(vaslog.WithTenantID(context.Background(), tenant.String())))

	var got struct {
		ID     string         `json:"id"`
		Secret map[string]any `json:"secret"`
	}
	stored := store.records[store.storeKey(tenant, "key-1")].Response
	if err := json.Unmarshal(stored, &got); err != nil {
		t.Fatalf("stored %s: %v", stored, err)
	}
	if v, ok := got.Secret["secret"]; !ok || v != nil || got.Secret["id"] != "s1" || got.Secret["last4"] != "labc" || got.ID != "c1" {
		t.Fatalf("stored %s: want only secret.secret null", stored)
	}
}
