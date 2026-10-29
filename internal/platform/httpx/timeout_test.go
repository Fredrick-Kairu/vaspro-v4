package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
)

func TestTimeout_SlowHandler_GetsSYS5030Instead(t *testing.T) {
	released := make(chan struct{})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
		// This write happens after the deadline; it must never reach the
		// client's response.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("too late"))
		close(released)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	httpx.Timeout(20 * time.Millisecond)(next).ServeHTTP(w, r)

	if w.Code != 503 {
		t.Fatalf("status = %d, want 503", w.Code)
	}

	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, w.Body.String())
	}
	if m["code"] != "SYS-5030" {
		t.Fatalf("code = %v, want SYS-5030", m["code"])
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}

	<-released // let the slow handler's goroutine finish before the test ends
	if w.Body.String() == "too late" {
		t.Fatal("the handler's late write leaked into the response")
	}
}

func TestTimeout_FastHandler_PassesThrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("on time"))
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	httpx.Timeout(time.Second)(next).ServeHTTP(w, r)

	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTeapot)
	}
	if w.Body.String() != "on time" {
		t.Fatalf("body = %q, want %q", w.Body.String(), "on time")
	}
}

func TestTimeout_HandlerPanics_PropagatesPanic(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	defer func() {
		if recover() == nil {
			t.Fatal("expected the panic to propagate so an outer Recover middleware can catch it")
		}
	}()
	httpx.Timeout(time.Second)(next).ServeHTTP(w, r)
}
