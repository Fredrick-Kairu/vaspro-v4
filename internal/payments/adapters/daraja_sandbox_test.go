//go:build integration && daraja_sandbox

package adapters_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
)

type envCreds struct{ creds adapters.DarajaCredentials }

func (e envCreds) DarajaCredentials(context.Context, uuid.UUID) (adapters.DarajaCredentials, error) {
	return e.creds, nil
}

// TestDarajaSandbox runs the adapter against Safaricom's real Daraja
// sandbox: OAuth, an STK push to the published sandbox test number,
// and STK queries until it ends or a minute passes. It runs nightly
// (.github/workflows/daraja-nightly.yml), never on a PR, and skips
// until the DARAJA_SANDBOX_* secrets exist.
func TestDarajaSandbox(t *testing.T) {
	creds := adapters.DarajaCredentials{
		ConsumerKey:    os.Getenv("DARAJA_SANDBOX_CONSUMER_KEY"),
		ConsumerSecret: os.Getenv("DARAJA_SANDBOX_CONSUMER_SECRET"),
		ShortCode:      os.Getenv("DARAJA_SANDBOX_SHORTCODE"),
		Passkey:        os.Getenv("DARAJA_SANDBOX_PASSKEY"),
	}
	if creds.ConsumerKey == "" || creds.ConsumerSecret == "" || creds.ShortCode == "" || creds.Passkey == "" {
		t.Skip("DARAJA_SANDBOX_* not set")
	}
	baseURL := os.Getenv("DARAJA_SANDBOX_BASE_URL")
	if baseURL == "" {
		baseURL = "https://sandbox.safaricom.co.ke"
	}
	callbackURL := os.Getenv("DARAJA_SANDBOX_CALLBACK_URL")
	if callbackURL == "" {
		callbackURL = "https://example.com/daraja-sandbox-callback" // nothing receives it; the query reads the outcome
	}
	nairobi, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Fatal(err)
	}
	client := adapters.NewDaraja(adapters.DarajaConfig{
		BaseURL: baseURL, Cache: testkit.StartRedis(t), Credentials: envCreds{creds},
		Location: nairobi, Clock: clock.Real{},
	})
	ctx := context.Background()
	account := uuid.New()

	resp, err := client.STKPush(ctx, adapters.STKPushRequest{
		RailAccountID: account, AmountMinor: 100, PhoneE164: "+" + daraja.SandboxMSISDN,
		AccountReference: "VASPROTEST", Description: "Nightly", CallbackURL: callbackURL,
	})
	if err != nil {
		t.Fatalf("STK push: %v", err)
	}
	if resp.CheckoutRequestID == "" {
		t.Fatal("no CheckoutRequestID")
	}

	deadline := time.Now().Add(time.Minute)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		q, err := client.STKQuery(ctx, account, resp.CheckoutRequestID)
		if err != nil {
			t.Fatalf("STK query: %v", err)
		}
		if !q.Pending {
			t.Logf("sandbox result %d (%s), code %q", q.ResultCode, q.ResultDesc, q.ErrorCode())
			return
		}
		if time.Now().After(deadline) {
			t.Log("still pending after a minute; the push and queries worked")
			return
		}
		<-tick.C
	}
}
