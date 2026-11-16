package domain_test

import (
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

func TestCheckURL(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", 2049-len("https://example.com/"))
	cases := []struct {
		name     string
		url      string
		insecure bool
		ok       bool
	}{
		{"a public https URL is allowed", "https://hooks.example.com/vaspro?x=1", false, true},
		{"a port is allowed", "https://hooks.example.com:8443/in", false, true},
		{"a public literal address is allowed", "https://203.0.113.10/in", false, true},
		{"exactly 2,048 characters is allowed", long[:2048], false, true},
		{"plain http is refused", "http://hooks.example.com/in", false, false},
		{"plain http is allowed in development", "http://hooks.example.com/in", true, true},
		{"other schemes are refused", "ftp://hooks.example.com/in", true, false},
		{"user information is refused", "https://user:pass@hooks.example.com/in", false, false},
		{"a user name alone is refused", "https://user@hooks.example.com/in", false, false},
		{"a fragment is refused", "https://hooks.example.com/in#frag", false, false},
		{"no host is refused", "https:///in", false, false},
		{"a relative URL is refused", "/in", false, false},
		{"garbage is refused", "::not a url", false, false},
		{"whitespace is refused", "https://hooks.example.com/a b", false, false},
		{"2,049 characters is refused", long, false, false},
		{"loopback is refused", "https://127.0.0.1/in", false, false},
		{"another loopback address is refused", "https://127.8.9.10/in", false, false},
		{"IPv6 loopback is refused", "https://[::1]/in", false, false},
		{"10/8 is refused", "https://10.1.2.3/in", false, false},
		{"172.16/12 is refused", "https://172.20.0.1/in", false, false},
		{"192.168/16 is refused", "https://192.168.1.1/in", false, false},
		{"cloud metadata is refused", "https://169.254.169.254/latest/meta-data", false, false},
		{"CGNAT is refused", "https://100.64.0.1/in", false, false},
		{"IPv6 unique-local metadata is refused", "https://[fd00:ec2::254]/in", false, false},
		{"IPv4-mapped loopback is refused", "https://[::ffff:127.0.0.1]/in", false, false},
		{"multicast is refused", "https://224.0.0.1/in", false, false},
		{"the unspecified address is refused", "https://0.0.0.0/in", false, false},
		{"a private address is allowed in development", "http://127.0.0.1:8080/in", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := domain.CheckURL(c.url, c.insecure)
			if c.ok && err != nil {
				t.Fatalf("CheckURL(%q) = %v, want allowed", c.url, err)
			}
			if !c.ok && !errors.Is(err, domain.ErrURLNotAllowed) {
				t.Fatalf("CheckURL(%q) = %v, want ErrURLNotAllowed", c.url, err)
			}
		})
	}
}

func TestAddressAllowed(t *testing.T) {
	cases := map[string]bool{
		"203.0.113.10":    true,
		"8.8.8.8":         true,
		"2001:4860::8888": true,
		"0.1.2.3":         false,
		"127.0.0.1":       false,
		"::1":             false,
		"::":              false,
		"10.0.0.1":        false,
		"172.31.255.255":  false,
		"192.168.0.1":     false,
		"169.254.0.1":     false,
		"fe80::1":         false,
		"100.127.0.1":     false,
		"192.0.0.8":       false,
		"198.18.0.1":      false,
		"240.0.0.1":       false,
		"255.255.255.255": false,
		"239.1.1.1":       false,
		"ff02::1":         false,
		"fc00::1":         false,
		"::ffff:10.0.0.1": false,
		"::ffff:8.8.8.8":  true,
	}
	for addr, want := range cases {
		if got := domain.AddressAllowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("AddressAllowed(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestCheckSubscriptions(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"one client event", []string{"wallet.credited"}, []string{"wallet.credited"}},
		{"several, sorted and deduplicated", []string{"wallet.credited", "tokens.purchased", "wallet.credited"}, []string{"tokens.purchased", "wallet.credited"}},
		{"everything", []string{"*"}, []string{"*"}},
		{"everything subsumes the rest", []string{"wallet.credited", "*"}, []string{"*"}},
		{"none is refused", nil, nil},
		{"an internal event is refused", []string{"ledger.rebuild_failed"}, nil},
		{"an unknown event is refused", []string{"wallet.debited"}, nil},
		{"a webhook-only type is refused", []string{"webhook.verification"}, nil},
		{"one bad among good is refused", []string{"wallet.credited", "recon.exception_opened"}, nil},
		{"a family wildcard is refused", []string{"wallet.*"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := domain.CheckSubscriptions(c.in)
			if c.want == nil {
				if !errors.Is(err, domain.ErrInvalidSubscriptions) {
					t.Fatalf("CheckSubscriptions(%v) = %v, %v, want ErrInvalidSubscriptions", c.in, got, err)
				}
				return
			}
			if err != nil || !slices.Equal(got, c.want) {
				t.Fatalf("CheckSubscriptions(%v) = %v, %v, want %v", c.in, got, err, c.want)
			}
		})
	}
}

func TestVerificationBody(t *testing.T) {
	id, tenant := uuid.New(), uuid.New()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	body, err := domain.VerificationBody(id, tenant, at, "chal-123")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id": id.String(), "type": "webhook.verification", "version": float64(1), "tenant_id": tenant.String(),
		"occurred_at": "2026-11-16T09:00:00Z", "data": map[string]any{"challenge": "chal-123"},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("body = %s, want %s", gotJSON, wantJSON)
	}
}

func TestNewChallenge(t *testing.T) {
	a, err := domain.NewChallenge(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := domain.NewChallenge(nil)
	if len(a) < 32 || a == b {
		t.Fatalf("challenges %q and %q: want long and distinct", a, b)
	}
	if _, err := domain.NewChallenge(strings.NewReader("short")); err == nil {
		t.Fatal("a short random source should error")
	}
}

func TestChallengeEchoed(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"the same value", `{"challenge":"abc"}`, true},
		{"with spaces and a newline", "{ \"challenge\": \"abc\" }\n", true},
		{"other fields alongside", `{"challenge":"abc","ok":true}`, true},
		{"another value", `{"challenge":"abd"}`, false},
		{"a prefix of it", `{"challenge":"ab"}`, false},
		{"empty", ``, false},
		{"not JSON", `abc`, false},
		{"the bare string", `"abc"`, false},
		{"a number", `{"challenge":1}`, false},
		{"another key", `{"Challenge2":"abc"}`, false},
		{"trailing junk", `{"challenge":"abc"} x`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := domain.ChallengeEchoed([]byte(c.body), "abc"); got != c.ok {
				t.Fatalf("ChallengeEchoed(%q) = %v, want %v", c.body, got, c.ok)
			}
		})
	}
}

func TestClientEventTypes_AreWellFormed(t *testing.T) {
	if len(domain.ClientEventTypes) == 0 {
		t.Fatal("no client event types")
	}
	for _, e := range domain.ClientEventTypes {
		if !domain.IsClientEventType(e) {
			t.Errorf("IsClientEventType(%q) = false", e)
		}
	}
	if domain.IsClientEventType("ledger.rebuild_failed") || domain.IsClientEventType("*") {
		t.Error("an internal type, or *, is not a client event type")
	}
}

func TestTestEventBody(t *testing.T) {
	id, tenant, endpoint := uuid.New(), uuid.New(), uuid.New()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	body, err := domain.TestEventBody(id, tenant, endpoint, at)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID         uuid.UUID `json:"id"`
		Type       string    `json:"type"`
		Version    int       `json:"version"`
		TenantID   uuid.UUID `json:"tenant_id"`
		OccurredAt time.Time `json:"occurred_at"`
		Data       struct {
			EndpointID uuid.UUID `json:"endpoint_id"`
			Message    string    `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Type != domain.TestEventType || got.Version != 1 || got.TenantID != tenant ||
		!got.OccurredAt.Equal(at) || got.Data.EndpointID != endpoint || got.Data.Message == "" {
		t.Fatalf("test event %+v", got)
	}
	if domain.IsClientEventType(domain.TestEventType) {
		t.Fatal("webhook.test is never on the bus, so it isn't a client event type")
	}
}
