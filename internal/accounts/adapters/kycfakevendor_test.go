package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A tester decides a check through the fake's own HTTP API (the dev
// environment's kycvendor command), naming it by our check ID; the
// webhook that arrives is one the adapter accepts.
func TestFakeVendorProvider_TesterDecision(t *testing.T) {
	type delivery struct {
		header http.Header
		body   []byte
	}
	received := make(chan delivery, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- delivery{r.Header.Clone(), body}
	}))
	t.Cleanup(hook.Close)

	vendor := kycvendor.New(testSecret)
	t.Cleanup(vendor.Close)
	vendor.SetWebhookURL(hook.URL)
	p := adapters.NewFakeVendorProvider(vendor.URL(), testSecret)
	checkID := uuid.New()
	started, err := p.StartCheck(context.Background(), adapters.KYCStart{CheckID: checkID, Kind: "individual", SubjectID: uuid.New()})
	if err != nil {
		t.Fatalf("StartCheck: %v", err)
	}

	resp, err := http.Post(vendor.URL()+"/checks/"+checkID.String()+"/decision", "application/json", strings.NewReader(`{"status":"passed"}`))
	if err != nil {
		t.Fatalf("decision: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decision status %d, want 200", resp.StatusCode)
	}
	d := <-received
	got, err := p.ParseWebhook(d.header, d.body, time.Now())
	if err != nil || got.ProviderRef != started.ProviderRef || got.Status != "passed" {
		t.Fatalf("webhook %+v, %v", got, err)
	}

	t.Run("the list shows the decision", func(t *testing.T) {
		resp, err := http.Get(vendor.URL() + "/checks")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var list []map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("decoding list: %v", err)
		}
		if len(list) != 1 || list[0]["reference"] != checkID.String() || list[0]["provider_ref"] != started.ProviderRef || list[0]["status"] != "passed" {
			t.Fatalf("list %v", list)
		}
	})
	t.Run("an unknown check or status is refused", func(t *testing.T) {
		for _, c := range []struct{ id, body string }{
			{uuid.NewString(), `{"status":"passed"}`},
			{checkID.String(), `{"status":"approved"}`},
		} {
			resp, err := http.Post(vendor.URL()+"/checks/"+c.id+"/decision", "application/json", strings.NewReader(c.body))
			if err != nil {
				t.Fatalf("decision: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode < 400 {
				t.Errorf("%s %s: status %d, want an error", c.id, c.body, resp.StatusCode)
			}
		}
	})
}
