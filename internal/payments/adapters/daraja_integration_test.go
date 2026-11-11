//go:build integration

package adapters_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
)

// Test-only credentials for the fake; nothing real.
var app = daraja.App{ConsumerKey: "fake-consumer-key", ConsumerSecret: "fake-consumer-secret-value", ShortCode: "600100", Passkey: "fake-passkey-value"}

type staticCreds struct {
	creds daraja.App
	err   error
}

func (s staticCreds) DarajaCredentials(context.Context, uuid.UUID) (adapters.DarajaCredentials, error) {
	return adapters.DarajaCredentials{ConsumerKey: s.creds.ConsumerKey, ConsumerSecret: s.creds.ConsumerSecret, ShortCode: s.creds.ShortCode, Passkey: s.creds.Passkey}, s.err
}

type harness struct {
	fake    *daraja.Fake
	cache   *cache.Cache
	client  *adapters.Daraja
	account uuid.UUID
}

// receiver records the bodies posted to it and answers Accepted.
type receiver struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	rc := &receiver{}
	rc.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.bodies = append(rc.bodies, b)
		rc.mu.Unlock()
		_, _ = w.Write(adapters.C2BResponse(true, ""))
	}))
	t.Cleanup(rc.server.Close)
	return rc
}

func (rc *receiver) all() [][]byte {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([][]byte(nil), rc.bodies...)
}

func newHarness(t *testing.T, opts ...func(*adapters.DarajaConfig)) *harness {
	t.Helper()
	f := daraja.New(app)
	t.Cleanup(f.Close)
	h := &harness{fake: f, cache: testkit.StartRedis(t), account: uuid.New()}
	cfg := adapters.DarajaConfig{
		BaseURL: f.URL(), Cache: h.cache, Credentials: staticCreds{creds: app},
		Location: nairobi, Clock: clock.Real{},
	}
	for _, o := range opts {
		o(&cfg)
	}
	h.client = adapters.NewDaraja(cfg)
	return h
}

func (h *harness) push(t *testing.T, callbackURL string) (adapters.STKPushResponse, error) {
	t.Helper()
	return h.client.STKPush(context.Background(), adapters.STKPushRequest{
		RailAccountID: h.account, AmountMinor: 150000, PhoneE164: "+" + daraja.SandboxMSISDN,
		AccountReference: "ABC123XY", Description: "Top-up", CallbackURL: callbackURL,
	})
}

func TestDaraja_STKOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		code int
		want problem.Code
	}{
		"success":            {daraja.ResultSuccess, ""},
		"cancelled by user":  {daraja.ResultCancelled, problem.PAY6503},
		"timeout":            {daraja.ResultTimeout, problem.PAY6504},
		"insufficient funds": {daraja.ResultInsufficientFunds, problem.PAY6505},
		"wrong PIN":          {daraja.ResultWrongPIN, problem.PAY6506},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			rc := newReceiver(t)
			h.fake.Next(daraja.Outcome{ResultCode: tc.code})

			resp, err := h.push(t, rc.server.URL)
			if err != nil || resp.CheckoutRequestID == "" || resp.MerchantRequestID == "" {
				t.Fatalf("push: %+v %v", resp, err)
			}
			h.fake.Wait()
			bodies := rc.all()
			if len(bodies) != 1 {
				t.Fatalf("callbacks %d", len(bodies))
			}
			cb, err := adapters.ParseSTKCallback(bodies[0], nairobi)
			if err != nil || cb.CheckoutRequestID != resp.CheckoutRequestID || cb.ResultCode != tc.code || cb.ErrorCode() != tc.want {
				t.Fatalf("callback %+v %v", cb, err)
			}
			if tc.code == daraja.ResultSuccess && (cb.AmountMinor != 150000 || cb.Receipt == "" || cb.PhoneE164 != "+"+daraja.SandboxMSISDN) {
				t.Fatalf("success callback %+v", cb)
			}

			q, err := h.client.STKQuery(context.Background(), h.account, resp.CheckoutRequestID)
			if err != nil || q.Pending || q.ResultCode != tc.code || q.ErrorCode() != tc.want || q.CheckoutRequestID != resp.CheckoutRequestID {
				t.Fatalf("query %+v %v", q, err)
			}
		})
	}
}

func TestDaraja_CallbackAmountDiffers(t *testing.T) {
	h := newHarness(t)
	rc := newReceiver(t)
	h.fake.Next(daraja.Outcome{PaidMinor: 100000})
	if _, err := h.push(t, rc.server.URL); err != nil {
		t.Fatal(err)
	}
	h.fake.Wait()
	cb, err := adapters.ParseSTKCallback(rc.all()[0], nairobi)
	if err != nil || cb.AmountMinor != 100000 {
		t.Fatalf("the callback's own amount is reported: %+v %v", cb, err)
	}
}

func TestDaraja_QueryPendingThenFinal(t *testing.T) {
	h := newHarness(t)
	rc := newReceiver(t)
	h.fake.Next(daraja.Outcome{ResultCode: daraja.ResultCancelled, Delivery: daraja.Manual})
	resp, err := h.push(t, rc.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	q, err := h.client.STKQuery(context.Background(), h.account, resp.CheckoutRequestID)
	if err != nil || !q.Pending || q.ErrorCode() != "" || q.Succeeded() {
		t.Fatalf("pending: %+v %v", q, err)
	}
	if _, err := h.fake.Release(resp.CheckoutRequestID); err != nil {
		t.Fatal(err)
	}
	q, err = h.client.STKQuery(context.Background(), h.account, resp.CheckoutRequestID)
	if err != nil || q.Pending || q.ResultCode != daraja.ResultCancelled {
		t.Fatalf("final: %+v %v", q, err)
	}
	if _, err := h.client.STKQuery(context.Background(), h.account, "ws_CO_unknown"); !errors.Is(err, adapters.ErrRailRefused) {
		t.Fatalf("unknown checkout: %v", err)
	}
}

func TestDaraja_TokenCache(t *testing.T) {
	t.Run("50 concurrent pushes share one token", func(t *testing.T) {
		h := newHarness(t)
		var wg sync.WaitGroup
		var failed atomic.Int32
		for range 50 {
			wg.Go(func() {
				if _, err := h.push(t, "http://127.0.0.1:1/cb"); err != nil {
					failed.Add(1)
				}
			})
		}
		wg.Wait()
		if failed.Load() != 0 || h.fake.TokenIssues() != 1 {
			t.Fatalf("failed %d, tokens issued %d, want 0 and 1", failed.Load(), h.fake.TokenIssues())
		}
	})
	t.Run("cached for its lifetime less a minute", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.push(t, "http://127.0.0.1:1/cb"); err != nil {
			t.Fatal(err)
		}
		keys, _ := h.cache.Keys(context.Background(), "*").Result()
		if len(keys) != 1 {
			t.Fatalf("keys %v", keys)
		}
		ttl, _ := h.cache.TTL(context.Background(), keys[0]).Result()
		if ttl > 3539*time.Second || ttl < 3500*time.Second {
			t.Fatalf("TTL %v, want about 3539s", ttl)
		}
		if strings.Contains(keys[0], app.ConsumerKey) {
			t.Fatalf("the cache key names the consumer key: %s", keys[0])
		}
	})
	t.Run("a token about to expire isn't cached", func(t *testing.T) {
		h := newHarness(t)
		h.fake.SetTokenTTL(60 * time.Second)
		for range 2 {
			if _, err := h.push(t, "http://127.0.0.1:1/cb"); err != nil {
				t.Fatal(err)
			}
		}
		if h.fake.TokenIssues() != 2 {
			t.Fatalf("issued %d, want 2", h.fake.TokenIssues())
		}
	})
	t.Run("a refused token is dropped and fetched again", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.push(t, "http://127.0.0.1:1/cb"); err != nil {
			t.Fatal(err)
		}
		h.fake.ExpireTokens()
		if _, err := h.push(t, "http://127.0.0.1:1/cb"); err != nil {
			t.Fatalf("after expiry: %v", err)
		}
		if _, err := h.push(t, "http://127.0.0.1:1/cb"); err != nil {
			t.Fatal(err)
		}
		if h.fake.TokenIssues() != 2 {
			t.Fatalf("issued %d, want 2 (the new token is cached)", h.fake.TokenIssues())
		}
	})
}

// A rail that issues tokens but refuses every call with them gets one
// retry with a fresh token, not a loop.
func TestDaraja_RetriesARefusedTokenOnce(t *testing.T) {
	var pushes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/oauth/") {
			_, _ = w.Write([]byte(`{"access_token":"tok-` + uuid.NewString() + `","expires_in":"3599"}`))
			return
		}
		pushes.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"requestId":"r","errorCode":"404.001.03","errorMessage":"Invalid Access Token"}`))
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, func(c *adapters.DarajaConfig) { c.BaseURL = srv.URL })
	if _, err := h.push(t, "http://127.0.0.1:1/cb"); !errors.Is(err, adapters.ErrRailRefused) || pushes.Load() != 2 {
		t.Fatalf("err %v after %d attempts, want ErrRailRefused after 2", err, pushes.Load())
	}
}

func TestDaraja_TransportErrors(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done() // never answers
		}))
		t.Cleanup(srv.Close)
		h := newHarness(t, func(c *adapters.DarajaConfig) {
			c.BaseURL = srv.URL
			c.HTTPClient = &http.Client{Timeout: 200 * time.Millisecond}
		})
		_, err := h.push(t, "http://127.0.0.1:1/cb")
		if !errors.Is(err, adapters.ErrRailTimeout) || adapters.ErrorCodeFor(err) != problem.PAY6502 {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		h := newHarness(t, func(c *adapters.DarajaConfig) { c.BaseURL = "http://127.0.0.1:1" })
		if _, err := h.push(t, "http://127.0.0.1:1/cb"); !errors.Is(err, adapters.ErrRailUnavailable) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/oauth/") {
				_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":"3599"}`))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		h := newHarness(t, func(c *adapters.DarajaConfig) { c.BaseURL = srv.URL })
		if _, err := h.push(t, "http://127.0.0.1:1/cb"); !errors.Is(err, adapters.ErrRailUnavailable) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("credentials unavailable", func(t *testing.T) {
		boom := errors.New("kms down")
		h := newHarness(t, func(c *adapters.DarajaConfig) { c.Credentials = staticCreds{err: boom} })
		if _, err := h.push(t, "http://127.0.0.1:1/cb"); !errors.Is(err, boom) || h.fake.TokenIssues() != 0 {
			t.Fatalf("err %v", err)
		}
	})
}

func TestDaraja_InvalidRequests(t *testing.T) {
	h := newHarness(t)
	for name, mutate := range map[string]func(*adapters.STKPushRequest){
		"amount with cents":    func(r *adapters.STKPushRequest) { r.AmountMinor = 150050 },
		"zero amount":          func(r *adapters.STKPushRequest) { r.AmountMinor = 0 },
		"not a Kenyan mobile":  func(r *adapters.STKPushRequest) { r.PhoneE164 = "+255712000000" },
		"no account reference": func(r *adapters.STKPushRequest) { r.AccountReference = "" },
		"reference over 12":    func(r *adapters.STKPushRequest) { r.AccountReference = "ABCDEFGHIJKLM" },
		"no callback URL":      func(r *adapters.STKPushRequest) { r.CallbackURL = "" },
	} {
		req := adapters.STKPushRequest{RailAccountID: h.account, AmountMinor: 150000, PhoneE164: "+" + daraja.SandboxMSISDN, AccountReference: "ABC123XY", Description: "Top-up", CallbackURL: "http://127.0.0.1:1/cb"}
		mutate(&req)
		if _, err := h.client.STKPush(context.Background(), req); !errors.Is(err, adapters.ErrInvalidRequest) {
			t.Errorf("%s: err %v, want ErrInvalidRequest", name, err)
		}
	}
	if h.fake.TokenIssues() != 0 {
		t.Fatal("an invalid request is refused before calling the rail")
	}
}

func TestDaraja_C2B(t *testing.T) {
	h := newHarness(t)
	val, conf := newReceiver(t), newReceiver(t)
	if err := h.client.RegisterC2BURLs(context.Background(), h.account, conf.server.URL, val.server.URL); err != nil {
		t.Fatal(err)
	}
	res, err := h.fake.Pay(daraja.C2BPayment{ShortCode: app.ShortCode, BillRef: "ABC123XY", AmountMinor: 250000, Phone: daraja.SandboxMSISDN})
	if err != nil || !res.Accepted {
		t.Fatalf("pay %+v %v", res, err)
	}
	for _, rc := range []*receiver{val, conf} {
		got, err := adapters.ParseC2BCallback(rc.all()[0], nairobi)
		if err != nil || got.Receipt != res.TransID || got.AmountMinor != 250000 || got.BillRef != "ABC123XY" || got.ShortCode != app.ShortCode {
			t.Fatalf("parsed %+v %v", got, err)
		}
	}
}

func TestDaraja_SecretsStayOut(t *testing.T) {
	wrong := app
	wrong.ConsumerSecret = "fake-wrong-secret-value"
	h := newHarness(t, func(c *adapters.DarajaConfig) { c.Credentials = staticCreds{creds: wrong} })
	_, err := h.push(t, "http://127.0.0.1:1/cb")
	if !errors.Is(err, adapters.ErrRailRefused) {
		t.Fatalf("err %v", err)
	}
	for _, secret := range []string{wrong.ConsumerSecret, app.Passkey, app.ConsumerKey} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the error carries a credential: %v", err)
		}
	}

	h = newHarness(t)
	h.fake.Next(daraja.Outcome{ResultCode: daraja.ResultWrongPIN, Delivery: daraja.NoCallback})
	resp, _ := h.push(t, "http://127.0.0.1:1/cb")
	keys, _ := h.cache.Keys(context.Background(), "*").Result()
	token, _ := h.cache.Get(context.Background(), keys[0]).Result()
	_, err = h.client.STKQuery(context.Background(), h.account, resp.CheckoutRequestID+"x")
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), app.Passkey) {
		t.Fatalf("a refused query's error must not carry the token or passkey: %v", err)
	}
}

// mutableCreds lets a test change the credentials between calls.
type mutableCreds struct {
	mu    sync.Mutex
	creds daraja.App
}

func (m *mutableCreds) set(a daraja.App) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creds = a
}

func (m *mutableCreds) DarajaCredentials(context.Context, uuid.UUID) (adapters.DarajaCredentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return adapters.DarajaCredentials{ConsumerKey: m.creds.ConsumerKey, ConsumerSecret: m.creds.ConsumerSecret, ShortCode: m.creds.ShortCode, Passkey: m.creds.Passkey}, nil
}

// CheckCredentials (task 4.3) proves the stored key and secret with a
// fresh OAuth call: a token cached from earlier credentials never
// passes for a changed secret.
func TestDaraja_CheckCredentials(t *testing.T) {
	creds := &mutableCreds{creds: app}
	h := newHarness(t, func(c *adapters.DarajaConfig) { c.Credentials = creds })
	ctx := context.Background()

	if err := h.client.CheckCredentials(ctx, h.account); err != nil {
		t.Fatalf("the right credentials: %v", err)
	}
	if _, err := h.push(t, "http://127.0.0.1:1/cb"); err != nil { // caches a token for this consumer key
		t.Fatal(err)
	}
	wrong := app
	wrong.ConsumerSecret = "fake-wrong-secret-value"
	creds.set(wrong)
	err := h.client.CheckCredentials(ctx, h.account)
	if !errors.Is(err, adapters.ErrRailRefused) {
		t.Fatalf("a wrong secret with a token still cached: %v, want ErrRailRefused", err)
	}
	if strings.Contains(err.Error(), wrong.ConsumerSecret) {
		t.Fatalf("the error carries the secret: %v", err)
	}
}
