package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireIPAllowlist(t *testing.T) {
	tests := []struct {
		name       string
		allowlist  []string
		remoteAddr string
		wantStatus int
	}{
		{"empty allowlist allows any IP", nil, "198.51.100.9:1234", http.StatusOK},
		{"IP inside an allowed CIDR", []string{"203.0.113.0/24"}, "203.0.113.42:1234", http.StatusOK},
		{"IP matching an allowed bare address", []string{"203.0.113.42"}, "203.0.113.42:1234", http.StatusOK},
		{"IP outside every allowed entry", []string{"203.0.113.0/24"}, "198.51.100.9:1234", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := requireIPAllowlist(okHandler())
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			req = req.WithContext(withClaims(req.Context(), Claims{ClientID: "c1", IPAllowlist: tc.allowlist}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("got status %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestRequireIPAllowlist_RejectionBody(t *testing.T) {
	handler := requireIPAllowlist(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req = req.WithContext(withClaims(req.Context(), Claims{ClientID: "c1", IPAllowlist: []string{"203.0.113.0/24"}}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var body struct {
		Code string `json:"code"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != "AUTH-4015" {
		t.Fatalf("got code %q, want AUTH-4015", body.Code)
	}
}
