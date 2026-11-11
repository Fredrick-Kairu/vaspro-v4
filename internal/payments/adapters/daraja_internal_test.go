package adapters

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
)

// The fake can only check a password is self-consistent, so the time
// zone Daraja expects is pinned here.
func TestPassword_UsesThePacksTimeZone(t *testing.T) {
	nairobi := time.FixedZone("EAT", 3*60*60)
	d := NewDaraja(DarajaConfig{Location: nairobi, Clock: clock.NewFake(time.Date(2026, 11, 11, 22, 30, 0, 0, time.UTC))})
	ts, password := d.password(DarajaCredentials{ShortCode: "600100", Passkey: "fake-passkey"})
	if ts != "20261112013000" {
		t.Fatalf("timestamp %s, want Nairobi time 20261112013000", ts)
	}
	if password != base64.StdEncoding.EncodeToString([]byte("600100fake-passkey20261112013000")) {
		t.Fatalf("password %s", password)
	}
}
