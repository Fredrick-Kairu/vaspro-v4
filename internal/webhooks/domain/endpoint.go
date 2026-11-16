package domain

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxURLLength is the longest endpoint URL (the schema's CHECK).
const MaxURLLength = 2048

// AllEvents subscribes an endpoint to every event sent to clients.
const AllEvents = "*"

// VerificationEventType is the challenge's type; it appears only in
// webhook bodies, never on the bus.
const VerificationEventType = "webhook.verification"

// ChallengeTimeout is how long an endpoint has to answer the challenge.
const ChallengeTimeout = 10 * time.Second

// SecretOverlap is how long a rotated-out secret still signs.
const SecretOverlap = 24 * time.Hour

var (
	// ErrURLNotAllowed: not a URL VASPRO will call (VAL-4019).
	ErrURLNotAllowed = errors.New("webhooks: URL not allowed")
	// ErrInvalidSubscriptions: no event types, or one not sent to
	// clients.
	ErrInvalidSubscriptions = errors.New("webhooks: invalid subscriptions")
)

// CheckURL checks an endpoint URL before it's stored: https (http only
// when allowInsecure, for development), a host, no user information or
// fragment, at most MaxURLLength characters, and, for a literal
// address, one AddressAllowed accepts. A name is checked again at every
// connection, after DNS (adapters.Sender).
func CheckURL(raw string, allowInsecure bool) error {
	if len(raw) > MaxURLLength || strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("%w: longer than %d characters or contains whitespace", ErrURLNotAllowed, MaxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: not a URL", ErrURLNotAllowed)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowInsecure:
	default:
		return fmt.Errorf("%w: https only", ErrURLNotAllowed)
	}
	if u.User != nil {
		return fmt.Errorf("%w: no user name or password in the URL", ErrURLNotAllowed)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("%w: no fragment", ErrURLNotAllowed)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: no host", ErrURLNotAllowed)
	}
	if addr, err := netip.ParseAddr(host); err == nil && !allowInsecure && !AddressAllowed(addr) {
		return fmt.Errorf("%w: not a public address", ErrURLNotAllowed)
	}
	return nil
}

// refusedPrefixes are the ranges no webhook may reach, beyond what
// netip.Addr's own predicates cover.
var refusedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
}

// AddressAllowed reports whether a webhook may connect to addr: not
// unspecified, loopback, private (RFC 1918, unique-local), link-local
// (cloud metadata's 169.254.169.254 included), multicast, or a reserved
// range. An IPv4-mapped IPv6 address is judged as its IPv4 address.
func AddressAllowed(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() {
		return false
	}
	for _, p := range refusedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// CheckSubscriptions checks the event types an endpoint subscribes to,
// returning them sorted and deduplicated. AllEvents stands for every
// client event, so it subsumes the rest.
func CheckSubscriptions(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: subscribe to at least one event type, or %q", ErrInvalidSubscriptions, AllEvents)
	}
	out := make([]string, 0, len(in))
	for _, t := range in {
		if t != AllEvents && !IsClientEventType(t) {
			return nil, fmt.Errorf("%w: %q isn't an event sent to clients", ErrInvalidSubscriptions, t)
		}
		out = append(out, t)
	}
	if slices.Contains(out, AllEvents) {
		return []string{AllEvents}, nil
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// NewChallenge is a random value an endpoint must echo, from r
// (crypto/rand when nil).
func NewChallenge(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("webhooks: generating a challenge: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// envelope is the body every webhook carries (data model §8): the bus
// event without its trace ID.
type envelope struct {
	ID         uuid.UUID `json:"id"`
	Type       string    `json:"type"`
	Version    int       `json:"version"`
	TenantID   uuid.UUID `json:"tenant_id"`
	OccurredAt time.Time `json:"occurred_at"`
	Data       any       `json:"data"`
}

// VerificationBody is the signed webhook.verification request's body.
func VerificationBody(id, tenant uuid.UUID, at time.Time, challenge string) ([]byte, error) {
	b, err := json.Marshal(envelope{
		ID: id, Type: VerificationEventType, Version: 1, TenantID: tenant, OccurredAt: at.UTC(),
		Data: map[string]string{"challenge": challenge},
	})
	if err != nil {
		return nil, fmt.Errorf("webhooks: encoding the challenge: %w", err)
	}
	return b, nil
}

// ChallengeEchoed reports whether an endpoint's answer is a JSON object
// whose "challenge" (that exact key) is the value sent.
func ChallengeEchoed(body []byte, challenge string) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil || dec.More() {
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		return false
	}
	raw, ok := obj["challenge"]
	if !ok {
		return false
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	return got == challenge
}

// TestEventType is a test event's type; it appears only in webhook
// bodies, never on the bus.
const TestEventType = "webhook.test"

// TestEventBody is a webhook.test request's body: the usual envelope,
// naming the endpoint it was sent to.
func TestEventBody(id, tenant, endpoint uuid.UUID, at time.Time) ([]byte, error) {
	b, err := json.Marshal(envelope{
		ID: id, Type: TestEventType, Version: 1, TenantID: tenant, OccurredAt: at.UTC(),
		Data: map[string]string{
			"endpoint_id": endpoint.String(),
			"message":     "A test event from VASPRO. Answer 2xx; it carries no change.",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("webhooks: encoding the test event: %w", err)
	}
	return b, nil
}
