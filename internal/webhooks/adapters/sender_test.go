package adapters_test

import (
	"context"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
)

func rootsOf(srv *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

func send(t *testing.T, s *adapters.Sender, url string) adapters.Response {
	t.Helper()
	return s.Send(context.Background(), adapters.Request{
		URL: url, Body: []byte(`{"id":"x"}`), Headers: map[string]string{"Vaspro-Webhook-Id": "x"},
	})
}

func TestSender_RefusesPrivateAddressesUnlessInsecure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]

	secure := adapters.NewSender(adapters.SenderConfig{RootCAs: rootsOf(srv)})
	for name, url := range map[string]string{
		"a literal loopback address":       srv.URL,
		"localhost, resolving to loopback": "https://localhost" + port + "/in",
	} {
		t.Run(name, func(t *testing.T) {
			got := send(t, secure, url)
			if got.Outcome != adapters.OutcomeRefusedURL || got.Error == "" {
				t.Fatalf("outcome = %q (%q), want refused_url", got.Outcome, got.Error)
			}
		})
	}
	t.Run("plain http without the development setting", func(t *testing.T) {
		if got := send(t, secure, "http://hooks.example.com/in"); got.Outcome != adapters.OutcomeRefusedURL {
			t.Fatalf("outcome = %q, want refused_url", got.Outcome)
		}
	})
	if n := hits.Load(); n != 0 {
		t.Fatalf("the server was reached %d times, want never", n)
	}

	insecure := adapters.NewSender(adapters.SenderConfig{RootCAs: rootsOf(srv), AllowInsecure: true})
	if got := send(t, insecure, srv.URL); got.Outcome != adapters.OutcomeSucceeded || got.Status != 200 {
		t.Fatalf("with the development setting: outcome = %q (%d, %q), want succeeded", got.Outcome, got.Status, got.Error)
	}
}

func TestSender_Outcomes(t *testing.T) {
	var gotBody, gotHeader, gotType string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotHeader, gotType = string(b), r.Header.Get("Vaspro-Webhook-Id"), r.Header.Get("Content-Type")
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("X-Seen", "yes")
			w.WriteHeader(204)
		case "/fail":
			w.WriteHeader(500)
			_, _ = io.WriteString(w, "boom")
		case "/big":
			_, _ = io.WriteString(w, strings.Repeat("a", 5000))
		case "/exact":
			_, _ = io.WriteString(w, strings.Repeat("a", 4096))
		case "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
	}))
	defer srv.Close()
	s := adapters.NewSender(adapters.SenderConfig{RootCAs: rootsOf(srv), AllowInsecure: true, Timeout: 300 * time.Millisecond})

	t.Run("a 2xx succeeds, sending the body and headers as given", func(t *testing.T) {
		got := send(t, s, srv.URL+"/ok")
		if got.Outcome != adapters.OutcomeSucceeded || got.Status != 204 || got.Headers.Get("X-Seen") != "yes" {
			t.Fatalf("got %+v", got)
		}
		if gotBody != `{"id":"x"}` || gotHeader != "x" || gotType != "application/json" {
			t.Fatalf("server saw body %q, id %q, type %q", gotBody, gotHeader, gotType)
		}
	})
	t.Run("a 500 is http_status with its body", func(t *testing.T) {
		got := send(t, s, srv.URL+"/fail")
		if got.Outcome != adapters.OutcomeHTTPStatus || got.Status != 500 || string(got.Body) != "boom" || got.Error != "" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("the body is kept to 4 KB", func(t *testing.T) {
		got := send(t, s, srv.URL+"/big")
		if len(got.Body) != 4096 || !got.Truncated {
			t.Fatalf("body %d bytes, truncated %v", len(got.Body), got.Truncated)
		}
		got = send(t, s, srv.URL+"/exact")
		if len(got.Body) != 4096 || got.Truncated {
			t.Fatalf("exactly 4 KB: body %d bytes, truncated %v", len(got.Body), got.Truncated)
		}
	})
	t.Run("a slow endpoint times out", func(t *testing.T) {
		start := time.Now()
		got := send(t, s, srv.URL+"/slow")
		if got.Outcome != adapters.OutcomeTimeout || got.Error == "" {
			t.Fatalf("got %q (%q), want timeout", got.Outcome, got.Error)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("took %v, want about the 300ms timeout", took)
		}
	})
	t.Run("nothing listening is connect", func(t *testing.T) {
		closed := httptest.NewTLSServer(http.NotFoundHandler())
		url := closed.URL
		closed.Close()
		if got := send(t, s, url); got.Outcome != adapters.OutcomeConnect || got.Error == "" {
			t.Fatalf("got %q (%q), want connect", got.Outcome, got.Error)
		}
	})
	t.Run("an untrusted certificate is tls", func(t *testing.T) {
		strict := adapters.NewSender(adapters.SenderConfig{AllowInsecure: true})
		if got := send(t, strict, srv.URL+"/ok"); got.Outcome != adapters.OutcomeTLS || got.Error == "" {
			t.Fatalf("got %q (%q), want tls", got.Outcome, got.Error)
		}
	})
}

func TestSender_NeverFollowsRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer redirector.Close()
	roots := rootsOf(redirector)
	roots.AddCert(target.Certificate())

	s := adapters.NewSender(adapters.SenderConfig{RootCAs: roots, AllowInsecure: true})
	got := send(t, s, redirector.URL)
	if got.Outcome != adapters.OutcomeHTTPStatus || got.Status != http.StatusFound {
		t.Fatalf("got %q %d, want http_status 302", got.Outcome, got.Status)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("the redirect target was hit %d times, want never", n)
	}
}

func TestSender_ErrorsCarryNoURL(t *testing.T) {
	s := adapters.NewSender(adapters.SenderConfig{AllowInsecure: true, Timeout: 300 * time.Millisecond})
	got := send(t, s, "https://127.0.0.1:1/secret-path?token=abc")
	if got.Outcome != adapters.OutcomeConnect {
		t.Fatalf("outcome = %q, want connect", got.Outcome)
	}
	if strings.Contains(got.Error, "secret-path") || strings.Contains(got.Error, "token=abc") {
		t.Fatalf("error %q names the URL", got.Error)
	}
}
