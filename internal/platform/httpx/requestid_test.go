package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
)

func TestRequestID_SetsHeaderAndContext(t *testing.T) {
	var sawID string
	var sawOK bool

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawID, sawOK = vaslog.RequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	httpx.RequestID(next).ServeHTTP(w, r)

	headerID := w.Header().Get("X-Request-Id")
	if headerID == "" {
		t.Fatal("expected X-Request-Id response header to be set")
	}
	if !sawOK {
		t.Fatal("expected the request ID to be readable from context inside the handler")
	}
	if sawID != headerID {
		t.Fatalf("context request ID %q does not match response header %q", sawID, headerID)
	}
}

func TestRequestID_DifferentPerRequest(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := httpx.RequestID(next)

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/", nil))

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/", nil))

	id1 := w1.Header().Get("X-Request-Id")
	id2 := w2.Header().Get("X-Request-Id")
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("expected two distinct non-empty request IDs, got %q and %q", id1, id2)
	}
}
