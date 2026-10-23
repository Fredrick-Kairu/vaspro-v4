package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestRequireScope(t *testing.T) {
	tests := []struct {
		name       string
		required   string
		claims     Claims
		wantStatus int
	}{
		{"no scope required, no claims scope", "", Claims{ClientID: "c1"}, http.StatusOK},
		{"required scope present", "sms:send", Claims{ClientID: "c1", Scopes: []string{"sms:send", "reports:read"}}, http.StatusOK},
		{"required scope missing", "sms:send", Claims{ClientID: "c1", Scopes: []string{"reports:read"}}, http.StatusForbidden},
		{"required scope with no scopes at all", "sms:send", Claims{ClientID: "c1"}, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := requireScope(tc.required)(okHandler())
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req = req.WithContext(withClaims(req.Context(), tc.claims))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("got status %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestRequireScope_MissingScopeBody(t *testing.T) {
	handler := requireScope("sms:send")(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(withClaims(req.Context(), Claims{ClientID: "c1"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var body struct {
		Code string `json:"code"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != "AUTH-4014" {
		t.Fatalf("got code %q, want AUTH-4014", body.Code)
	}
}

func TestRequireScope_NoClaimsInContext_IsWiringBug(t *testing.T) {
	handler := requireScope("sms:send")(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500 (requireJWT must run before requireScope)", rec.Code)
	}
}
