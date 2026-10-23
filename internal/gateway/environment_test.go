package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireLiveEnvironment(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		wantStatus int
	}{
		{"live credential allowed", "live", http.StatusOK},
		{"sandbox credential rejected", "sandbox", http.StatusForbidden},
		{"empty environment rejected", "", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := requireLiveEnvironment(okHandler())
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req = req.WithContext(withClaims(req.Context(), Claims{ClientID: "c1", Environment: tc.env}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("got status %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestRequireLiveEnvironment_RejectionBody(t *testing.T) {
	handler := requireLiveEnvironment(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(withClaims(req.Context(), Claims{ClientID: "c1", Environment: "sandbox"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var body struct {
		Code string `json:"code"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != "AUTH-4016" {
		t.Fatalf("got code %q, want AUTH-4016", body.Code)
	}
}
