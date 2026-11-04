package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestResolveClientIP(t *testing.T) {
	const proxy = "10.213.0.10/32"
	tests := []struct {
		name       string
		trusted    []string
		remoteAddr string
		xff        []string // one entry per X-Forwarded-For header
		want       string
	}{
		{"no trusted proxies: the peer, whatever the header says", nil, "198.51.100.9:1234", []string{"203.0.113.7"}, "198.51.100.9"},
		{"a trusted proxy: the client it names", []string{proxy}, "10.213.0.10:5000", []string{"203.0.113.7"}, "203.0.113.7"},
		{"an untrusted peer can't claim another IP", []string{proxy}, "198.51.100.9:1234", []string{"203.0.113.7"}, "198.51.100.9"},
		{"a client-sent entry before the real one is ignored", []string{proxy}, "10.213.0.10:5000", []string{"192.0.2.66, 203.0.113.7"}, "203.0.113.7"},
		{"trusted hops are skipped from the right", []string{proxy, "10.0.0.0/8"}, "10.213.0.10:5000", []string{"203.0.113.7, 10.1.2.3"}, "203.0.113.7"},
		{"several headers read as one list", []string{proxy}, "10.213.0.10:5000", []string{"192.0.2.66", "203.0.113.7"}, "203.0.113.7"},
		{"every hop trusted: the leftmost", []string{proxy, "10.0.0.0/8"}, "10.213.0.10:5000", []string{"10.9.9.9, 10.1.2.3"}, "10.9.9.9"},
		{"a malformed entry: the peer", []string{proxy}, "10.213.0.10:5000", []string{"203.0.113.7, not-an-ip"}, "10.213.0.10"},
		{"a trusted proxy without the header: the peer", []string{proxy}, "10.213.0.10:5000", nil, "10.213.0.10"},
		{"IPv6 client", []string{proxy}, "10.213.0.10:5000", []string{"2001:db8::7"}, "2001:db8::7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trusted, err := parseIPAllowlist(tc.trusted)
			if err != nil {
				t.Fatalf("parsing trusted proxies: %v", err)
			}
			var got netip.Addr
			var ok bool
			h := resolveClientIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got, ok = callerIP(r)
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if !ok || got.String() != tc.want {
				t.Fatalf("callerIP = %v (%v), want %s", got, ok, tc.want)
			}
		})
	}
}

// Behind a trusted proxy, the allowlist and the rate limit see the
// client, not the proxy.
func TestResolveClientIP_FeedsTheIPChecks(t *testing.T) {
	trusted, _ := parseIPAllowlist([]string{"10.213.0.10"})
	handler := resolveClientIP(trusted)(requireIPAllowlist(okHandler()))
	for _, tc := range []struct {
		xff  string
		want int
	}{
		{"203.0.113.42", http.StatusOK},
		{"198.51.100.9", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.213.0.10:5000"
		req.Header.Set("X-Forwarded-For", tc.xff)
		req = req.WithContext(withClaims(req.Context(), Claims{ClientID: "c1", IPAllowlist: []string{"203.0.113.0/24"}}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("client %s: status %d, want %d", tc.xff, rec.Code, tc.want)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.213.0.10:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.42")
	var key string
	resolveClientIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		key, _ = rateLimitByIP(r)
	})).ServeHTTP(httptest.NewRecorder(), req)
	if key != "ip:203.0.113.42" {
		t.Errorf("rate-limit key %q, want ip:203.0.113.42", key)
	}
}
