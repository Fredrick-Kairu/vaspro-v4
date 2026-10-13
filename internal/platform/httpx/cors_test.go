package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
)

func testCORSConfig() httpx.CORSConfig {
	return httpx.CORSConfig{
		AllowedOrigins: []string{"https://console.vaspro.io"},
		AllowedMethods: []string{"GET", "POST"},
		AllowedHeaders: []string{"Authorization", "Content-Type"},
	}
}

func TestCORS_AllowedOrigin_GetsHeadersEchoedBack(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://console.vaspro.io")
	w := httptest.NewRecorder()

	httpx.CORS(testCORSConfig())(next).ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://console.vaspro.io" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want the echoed origin", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want %q", got, "Origin")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (request should pass through)", w.Code)
	}
}

func TestCORS_DisallowedOrigin_GetsNoCORSHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()

	httpx.CORS(testCORSConfig())(next).ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected no Access-Control-Allow-Origin for a disallowed origin, got %q", got)
	}
}

func TestCORS_NoWildcardEverIssued(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://console.vaspro.io")
	w := httptest.NewRecorder()

	httpx.CORS(testCORSConfig())(next).ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Fatal("strict CORS must never issue a wildcard origin")
	}
}

func TestCORS_PreflightFromAllowedOrigin(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the inner handler should not run for an OPTIONS preflight")
	})

	r := httptest.NewRequest(http.MethodOptions, "/", nil)
	r.Header.Set("Origin", "https://console.vaspro.io")
	r.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()

	httpx.CORS(testCORSConfig())(next).ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("expected Access-Control-Allow-Methods on a preflight from an allowed origin")
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Fatal("expected Access-Control-Allow-Headers on a preflight from an allowed origin")
	}
}

func TestCORS_PreflightFromDisallowedOrigin_NoAllowHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the inner handler should not run for an OPTIONS preflight")
	})

	r := httptest.NewRequest(http.MethodOptions, "/", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	r.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()

	httpx.CORS(testCORSConfig())(next).ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Fatalf("expected no Access-Control-Allow-Methods for a disallowed origin, got %q", got)
	}
}
