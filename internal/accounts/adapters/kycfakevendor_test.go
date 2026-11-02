package adapters_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/kycvendor"
)

var testSecret = []byte("kyc-webhook-test-secret")

func TestFakeVendorProvider(t *testing.T) {
	vendor := kycvendor.New(testSecret)
	t.Cleanup(vendor.Close)
	p := adapters.NewFakeVendorProvider(vendor.URL(), testSecret)
	ctx := context.Background()

	if p.Name() != "fake" {
		t.Fatalf("name %q, want fake", p.Name())
	}
	started, err := p.StartCheck(ctx, adapters.KYCStart{CheckID: uuid.New(), Kind: "individual", SubjectID: uuid.New()})
	if err != nil || started.ProviderRef == "" || started.URL == "" {
		t.Fatalf("StartCheck: %+v, %v", started, err)
	}

	t.Run("a signed result is read", func(t *testing.T) {
		h, body := vendor.Webhook(started.ProviderRef, "passed", kycvendor.NoTamper)
		got, err := p.ParseWebhook(h, body, time.Now())
		if err != nil || got.ProviderRef != started.ProviderRef || got.Status != "passed" || got.Summary["document_type"] != "national_id" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	for name, tamper := range map[string]kycvendor.Tamper{
		"a body changed after signing": kycvendor.TamperBody,
		"another secret":               kycvendor.WrongSecret,
		"a timestamp 10 minutes old":   kycvendor.Stale,
		"a timestamp 10 minutes ahead": kycvendor.Future,
		"no signature":                 kycvendor.Unsigned,
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			h, body := vendor.Webhook(started.ProviderRef, "passed", tamper)
			if _, err := p.ParseWebhook(h, body, time.Now()); !errors.Is(err, adapters.ErrBadWebhookSignature) {
				t.Fatalf("got %v, want ErrBadWebhookSignature", err)
			}
		})
	}
	t.Run("a result for an unknown status is refused", func(t *testing.T) {
		h, body := vendor.Webhook(started.ProviderRef, "approved", kycvendor.NoTamper)
		if _, err := p.ParseWebhook(h, body, time.Now()); err == nil {
			t.Fatal("an unknown status was accepted")
		}
	})
	t.Run("the report", func(t *testing.T) {
		report, err := p.FetchReport(ctx, started.ProviderRef)
		if err != nil || report["status"] != "pending" {
			t.Fatalf("got %v, %v", report, err)
		}
	})
}
