package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// identityOpenAPIPath is docs/api/identity.yaml, relative to this
// package — the real spec, not a fixture, so this test proves the
// middleware actually understands identity's own contract.
const identityOpenAPIPath = "../../docs/api/identity.yaml"

func TestRequireValidRequestShape(t *testing.T) {
	spec, err := loadOpenAPISpec(identityOpenAPIPath)
	if err != nil {
		t.Fatalf("loadOpenAPISpec: %v", err)
	}
	handler := requireValidRequestShape(spec)(okHandler())

	tests := []struct {
		name           string
		method         string
		path           string
		body           string
		idempotencyKey string
		wantStatus     int
	}{
		{
			name:           "valid signup request",
			method:         http.MethodPost,
			path:           "/v1/auth/signups",
			body:           `{"email":"ada@example.com","full_name":"Ada Lovelace"}`,
			idempotencyKey: "key-1",
			wantStatus:     http.StatusOK,
		},
		{
			name:           "missing required field",
			method:         http.MethodPost,
			path:           "/v1/auth/signups",
			body:           `{"email":"ada@example.com"}`,
			idempotencyKey: "key-1",
			wantStatus:     http.StatusBadRequest,
		},
		{
			name:           "wrong field type",
			method:         http.MethodPost,
			path:           "/v1/auth/signups",
			body:           `{"email":"ada@example.com","full_name":12345}`,
			idempotencyKey: "key-1",
			wantStatus:     http.StatusBadRequest,
		},
		{
			name:           "missing required Idempotency-Key header",
			method:         http.MethodPost,
			path:           "/v1/auth/signups",
			body:           `{"email":"ada@example.com","full_name":"Ada Lovelace"}`,
			idempotencyKey: "",
			wantStatus:     http.StatusBadRequest,
		},
		{
			name:       "path not in the spec at all",
			method:     http.MethodPost,
			path:       "/v1/not-a-real-path",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "no-body GET against a real path is fine",
			method:     http.MethodGet,
			path:       "/.well-known/jwks.json",
			body:       "",
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body *bytes.Reader
			if tc.body != "" {
				body = bytes.NewReader([]byte(tc.body))
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			if tc.idempotencyKey != "" {
				req.Header.Set("Idempotency-Key", tc.idempotencyKey)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("got status %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestRequireValidRequestShape_RejectionBody(t *testing.T) {
	spec, err := loadOpenAPISpec(identityOpenAPIPath)
	if err != nil {
		t.Fatalf("loadOpenAPISpec: %v", err)
	}
	handler := requireValidRequestShape(spec)(okHandler())

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/signups", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
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
