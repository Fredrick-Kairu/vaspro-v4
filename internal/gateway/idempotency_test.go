package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
)

func TestRequireIdempotencyKeyHeader(t *testing.T) {
	handler := requireIdempotencyKeyHeader(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		method     string
		path       string
		header     string
		wantStatus int
	}{
		{"GET with no header runs anyway", http.MethodGet, "/v1/auth/sessions", "", http.StatusOK},
		{"POST with no header is rejected", http.MethodPost, "/v1/auth/signups", "", http.StatusBadRequest},
		{"POST with header runs", http.MethodPost, "/v1/auth/signups", "key-1", http.StatusOK},
		{"DELETE with no header is rejected", http.MethodDelete, "/v1/auth/sessions/abc", "", http.StatusBadRequest},
		{"exempt oauth token path runs without a header", http.MethodPost, "/v1/oauth/token", "", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.header != "" {
				req.Header.Set(idempotency.HeaderName, tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("got status %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestRequireIdempotencyKeyHeader_RejectionBody(t *testing.T) {
	handler := requireIdempotencyKeyHeader(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/signups", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var body struct {
		Code string `json:"code"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != "VAL-4000" {
		t.Fatalf("got code %q, want VAL-4000", body.Code)
	}
}
