package httpx_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
)

func TestRecover_PanicBecomesSYS9000WithNoStackTraceInBody(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something went very wrong, here is a secret: sk_live_abc123") // gitleaks:allow (fake secret under test)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	httpx.Recover(logger)(next).ServeHTTP(w, r)

	if w.Code != 500 {
		t.Fatalf("status = %d, want 500", w.Code)
	}

	body := w.Body.String()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, body)
	}
	if m["code"] != "SYS-9000" {
		t.Fatalf("code = %v, want SYS-9000", m["code"])
	}
	if strings.Contains(body, "secret") || strings.Contains(body, "sk_live") {
		t.Fatalf("response body leaked the panic value: %s", body)
	}
	if strings.Contains(body, ".go:") {
		t.Fatalf("response body appears to contain a stack trace: %s", body)
	}

	// The panic value and stack trace belong in the log, not the response.
	if !strings.Contains(logBuf.String(), "sk_live_abc123") {
		t.Fatalf("expected the panic value to be logged, log output: %s", logBuf.String())
	}
}

func TestRecover_NoPanic_PassesThrough(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("fine"))
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	httpx.Recover(logger)(next).ServeHTTP(w, r)

	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTeapot)
	}
	if w.Body.String() != "fine" {
		t.Fatalf("body = %q, want %q", w.Body.String(), "fine")
	}
	if logBuf.Len() != 0 {
		t.Fatalf("expected nothing logged when there's no panic, got: %s", logBuf.String())
	}
}
