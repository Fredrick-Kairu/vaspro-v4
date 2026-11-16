package domain_test

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

// Test secrets: the bytes 0..31, 32..63 and 64..95 in base64url,
// made up for these tests and never used anywhere real.
const (
	secretA = "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" //nolint:gosec // G101: a made-up test secret (bytes 0..31). gitleaks:allow
	secretB = "whsec_ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8" //nolint:gosec // G101: a made-up test secret (bytes 32..63). gitleaks:allow
	secretC = "whsec_QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8" //nolint:gosec // G101: a made-up test secret (bytes 64..95). gitleaks:allow
)

var (
	signedAt = time.Unix(1762941600, 0)
	body     = []byte(`{"id":"evt_1","type":"wallet.credited"}`)
)

// TestSign_GoldenVector pins the scheme against a value computed
// outside Go: printf '%s' '1762941600.<body>' | openssl dgst -sha256
// -hmac "$secretA", so the key is the whole secret string (OQ-23).
func TestSign_GoldenVector(t *testing.T) {
	sig, err := domain.Sign([]string{secretA}, signedAt, body)
	if err != nil {
		t.Fatal(err)
	}
	if sig.Timestamp != "1762941600" {
		t.Errorf("timestamp %q", sig.Timestamp)
	}
	if want := "v1=68fc2a246be4e54f9594a280dacb0de9f0bbfe23573fac43df457366a50850e3"; sig.Value != want {
		t.Errorf("signature\n got %s\nwant %s", sig.Value, want)
	}
}

func TestSignVerify_RoundTrip(t *testing.T) {
	sig := mustSign(t, []string{secretA}, signedAt)
	if err := domain.Verify(secretA, sig.Timestamp, sig.Value, body, signedAt); err != nil {
		t.Fatalf("own signature refused: %v", err)
	}
	if err := domain.Verify(secretA, sig.Timestamp, sig.Value, []byte{}, signedAt); !errors.Is(err, domain.ErrBadSignature) {
		t.Errorf("empty body: %v", err)
	}
}

// Every single-byte change to the body, timestamp or secret is refused.
func TestVerify_AnyChangedByteRefused(t *testing.T) {
	sig := mustSign(t, []string{secretA}, signedAt)

	for i := range body {
		changed := bytes.Clone(body)
		changed[i] ^= 0x01
		if err := domain.Verify(secretA, sig.Timestamp, sig.Value, changed, signedAt); !errors.Is(err, domain.ErrBadSignature) {
			t.Errorf("body byte %d changed: %v", i, err)
		}
	}
	for _, extra := range [][]byte{append(bytes.Clone(body), ' '), append([]byte{' '}, body...), body[:len(body)-1]} {
		if err := domain.Verify(secretA, sig.Timestamp, sig.Value, extra, signedAt); !errors.Is(err, domain.ErrBadSignature) {
			t.Errorf("body %q: %v", extra, err)
		}
	}

	// Changing the last digits keeps the timestamp inside the tolerance,
	// so it is the signature that refuses it.
	for i := range sig.Timestamp {
		ts := []byte(sig.Timestamp)
		ts[i] = '0' + (ts[i]-'0'+1)%10
		n, _ := strconv.ParseInt(string(ts), 10, 64)
		if err := domain.Verify(secretA, string(ts), sig.Value, body, time.Unix(n, 0)); !errors.Is(err, domain.ErrBadSignature) {
			t.Errorf("timestamp digit %d changed (%s): %v", i, ts, err)
		}
	}

	for i := len(domain.SecretPrefix); i < len(secretA); i++ {
		s := []byte(secretA)
		if s[i] == 'A' {
			s[i] = 'B'
		} else {
			s[i] = 'A'
		}
		if err := domain.Verify(string(s), sig.Timestamp, sig.Value, body, signedAt); !errors.Is(err, domain.ErrBadSignature) {
			t.Errorf("secret byte %d changed: %v", i, err)
		}
	}

	hexPart := strings.TrimPrefix(sig.Value, "v1=")
	for i := range hexPart {
		h := []byte(hexPart)
		if h[i] == '0' {
			h[i] = '1'
		} else {
			h[i] = '0'
		}
		if err := domain.Verify(secretA, sig.Timestamp, "v1="+string(h), body, signedAt); !errors.Is(err, domain.ErrBadSignature) {
			t.Errorf("signature digit %d changed: %v", i, err)
		}
	}
}

func TestVerify_Timestamps(t *testing.T) {
	sig := mustSign(t, []string{secretA}, signedAt)
	tol := domain.SignatureTolerance
	for _, c := range []struct {
		name string
		now  time.Time
		ok   bool
	}{
		{"at once", signedAt, true},
		{"exactly 5 minutes later", signedAt.Add(tol), true},
		{"a second too late", signedAt.Add(tol + time.Second), false},
		{"a day later", signedAt.Add(24 * time.Hour), false},
		{"signed exactly 5 minutes ahead of the receiver", signedAt.Add(-tol), true},
		{"signed a second too far ahead", signedAt.Add(-tol - time.Second), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := domain.Verify(secretA, sig.Timestamp, sig.Value, body, c.now)
			if c.ok && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !c.ok && !errors.Is(err, domain.ErrBadTimestamp) {
				t.Errorf("got %v, want ErrBadTimestamp", err)
			}
		})
	}

	for _, ts := range []string{"", "abc", "-1", "+1762941600", " 1762941600", "1762941600 ", "1762941600.0", "0x6913", "99999999999999999999"} {
		if err := domain.Verify(secretA, ts, sig.Value, body, signedAt); !errors.Is(err, domain.ErrBadTimestamp) {
			t.Errorf("timestamp %q: %v, want ErrBadTimestamp", ts, err)
		}
	}
}

// During a rotation both secrets sign, the current one first, and a
// client holding either accepts the request.
func TestSign_Rotation(t *testing.T) {
	sig := mustSign(t, []string{secretB, secretA}, signedAt)
	parts := strings.Split(sig.Value, " ")
	if len(parts) != 2 {
		t.Fatalf("two secrets gave %q", sig.Value)
	}
	if only := mustSign(t, []string{secretB}, signedAt); parts[0] != only.Value {
		t.Errorf("the current secret's signature isn't first: %q", sig.Value)
	}
	for _, s := range []string{secretA, secretB} {
		if err := domain.Verify(s, sig.Timestamp, sig.Value, body, signedAt); err != nil {
			t.Errorf("a client with %s… refused: %v", s[:10], err)
		}
	}
	if err := domain.Verify(secretC, sig.Timestamp, sig.Value, body, signedAt); !errors.Is(err, domain.ErrBadSignature) {
		t.Errorf("a third secret: %v", err)
	}
	// After the overlap only the new secret signs.
	after := mustSign(t, []string{secretB}, signedAt)
	if err := domain.Verify(secretA, after.Timestamp, after.Value, body, signedAt); !errors.Is(err, domain.ErrBadSignature) {
		t.Errorf("the old secret after the overlap: %v", err)
	}
}

func TestSign_Refusals(t *testing.T) {
	for name, secrets := range map[string][]string{
		"no secret":      nil,
		"three secrets":  {secretA, secretB, secretC},
		"the same twice": {secretA, secretA},
		"no prefix":      {strings.TrimPrefix(secretA, domain.SecretPrefix)},
		"too short":      {secretA[:len(secretA)-1]},
		"padded":         {secretA + "="},
		"not base64url":  {secretA[:len(secretA)-1] + "+"},
		// Same 32 bytes as secretA, but the last character's two
		// padding bits aren't zero: not the canonical spelling.
		"non-canonical":   {secretA[:len(secretA)-1] + "9"},
		"empty":           {""},
		"a valid and bad": {secretA, "whsec_"},
	} {
		if _, err := domain.Sign(secrets, signedAt, body); !errors.Is(err, domain.ErrInvalidSecret) {
			t.Errorf("%s: %v, want ErrInvalidSecret", name, err)
		}
	}
}

func TestVerify_MalformedSignatureHeader(t *testing.T) {
	sig := mustSign(t, []string{secretA}, signedAt)
	hexPart := strings.TrimPrefix(sig.Value, "v1=")
	for _, v := range []string{
		"",
		"v1=",
		hexPart,                    // no scheme
		"v2=" + hexPart,            // unknown scheme only
		"V1=" + hexPart,            // schemes are lower case
		"v1=" + hexPart[:62],       // truncated
		"v1=" + hexPart + "00",     // too long
		"v1=" + hexPart[:63] + "g", // not hex
		"v1=" + hexPart + ",",      // comma-separated isn't the format
	} {
		if err := domain.Verify(secretA, sig.Timestamp, v, body, signedAt); !errors.Is(err, domain.ErrBadSignature) {
			t.Errorf("signature %q: %v, want ErrBadSignature", v, err)
		}
	}
	// Unknown schemes beside a good v1 are ignored, and upper-case hex
	// still matches.
	for _, v := range []string{"v2=abc " + sig.Value, sig.Value + " v0=zz", "v1=" + strings.ToUpper(hexPart)} {
		if err := domain.Verify(secretA, sig.Timestamp, v, body, signedAt); err != nil {
			t.Errorf("signature %q refused: %v", v, err)
		}
	}
	// A malformed secret on the receiving side can never match.
	if err := domain.Verify("whsec_", sig.Timestamp, sig.Value, body, signedAt); !errors.Is(err, domain.ErrInvalidSecret) {
		t.Errorf("a malformed secret: %v", err)
	}
}

func TestGenerateSecret(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		s, err := domain.GenerateSecret(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s, domain.SecretPrefix) || len(s) != 49 || !domain.ValidSecret(s) {
			t.Fatalf("generated %q", s)
		}
		if seen[s] {
			t.Fatalf("repeated %q", s)
		}
		seen[s] = true
	}
	fixed, err := domain.GenerateSecret(bytes.NewReader(bytes.Repeat([]byte{0}, 32)))
	if err != nil || fixed != "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("from zero bytes: %q, %v", fixed, err)
	}
	if _, err := domain.GenerateSecret(iotest.ErrReader(errors.New("no entropy"))); err == nil {
		t.Error("a failing random source gave a secret")
	}
	if _, err := domain.GenerateSecret(bytes.NewReader(make([]byte, 31))); err == nil {
		t.Error("31 random bytes gave a secret")
	}
	for _, s := range []string{secretA, secretB, secretC} {
		if !domain.ValidSecret(s) {
			t.Errorf("%s… not valid", s[:10])
		}
	}
}

func mustSign(t *testing.T, secrets []string, at time.Time) domain.Signature {
	t.Helper()
	sig, err := domain.Sign(secrets, at, body)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}
