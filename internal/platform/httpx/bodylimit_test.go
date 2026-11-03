package httpx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
)

func TestBodyLimit_RejectsOversizedBody(t *testing.T) {
	var readErr error

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	body := strings.NewReader(strings.Repeat("x", 100))
	r := httptest.NewRequest(http.MethodPost, "/", body)
	w := httptest.NewRecorder()

	httpx.BodyLimit(10)(next).ServeHTTP(w, r)

	if readErr == nil {
		t.Fatal("expected reading an oversized body to error")
	}
}

func TestBodyLimit_AllowsBodyWithinLimit(t *testing.T) {
	var got string
	var readErr error

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		got, readErr = string(b), err
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	w := httptest.NewRecorder()

	httpx.BodyLimit(10)(next).ServeHTTP(w, r)

	if readErr != nil {
		t.Fatalf("unexpected error reading a body within the limit: %v", readErr)
	}
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestBodyLimitFunc_PicksTheLimitPerRequest(t *testing.T) {
	limit := httpx.BodyLimitFunc(func(r *http.Request) int64 {
		if r.URL.Path == "/upload" {
			return 100
		}
		return 10
	})
	read := func(path string, size int) error {
		var readErr error
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, readErr = io.ReadAll(r.Body) })
		limit(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", size))))
		return readErr
	}
	if err := read("/upload", 100); err != nil {
		t.Fatalf("100 bytes to the larger route: %v", err)
	}
	if err := read("/upload", 101); err == nil {
		t.Fatal("101 bytes to the larger route were read")
	}
	if err := read("/other", 11); err == nil {
		t.Fatal("11 bytes to a 10-byte route were read")
	}
}
