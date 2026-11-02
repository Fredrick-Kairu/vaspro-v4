// Package kycvendor fakes a KYC vendor for tests and local development
// (task 2.6, ADR 0008): a real HTTP server that starts checks, serves
// reports, and, when a test decides a check, posts a signed result
// webhook, so accounts' real adapter and webhook route run unchanged
// against it. Nothing here touches personal data: a "check" is just a
// reference.
package kycvendor

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Header names the fake signs its webhooks with.
const (
	HeaderTimestamp = "X-FakeKYC-Timestamp"
	HeaderSignature = "X-FakeKYC-Signature"
)

// Tamper breaks one thing about a webhook, for refusal tests.
type Tamper int

const (
	NoTamper    Tamper = iota
	TamperBody         // body changed after signing
	WrongSecret        // signed with another secret
	Stale              // timestamp 10 minutes old
	Future             // timestamp 10 minutes ahead
	Unsigned           // no signature header
)

// Vendor is the fake vendor's server.
type Vendor struct {
	server *httptest.Server
	secret []byte

	mu         sync.Mutex
	webhookURL string
	checks     map[string]string // provider_ref → status
}

// New starts the fake, signing webhooks with secret. Close it when done.
func New(secret []byte) *Vendor {
	v := &Vendor{secret: secret, checks: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /checks", v.startCheck)
	mux.HandleFunc("GET /checks/{ref}/report", v.report)
	v.server = httptest.NewServer(mux)
	return v
}

// URL is the vendor's base URL (accounts' ACCOUNTS_KYC_FAKE_URL).
func (v *Vendor) URL() string { return v.server.URL }

// Close stops the server.
func (v *Vendor) Close() { v.server.Close() }

// SetWebhookURL is where Decide posts results.
func (v *Vendor) SetWebhookURL(url string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.webhookURL = url
}

func (v *Vendor) startCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reference string `json:"reference"`
		Kind      string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Reference == "" || req.Kind == "" {
		http.Error(w, "reference and kind are required", http.StatusBadRequest)
		return
	}
	ref := "fkyc_" + uuid.NewString()
	v.mu.Lock()
	v.checks[ref] = "pending"
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"provider_ref": ref, "url": v.server.URL + "/flow/" + ref})
}

func (v *Vendor) report(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	v.mu.Lock()
	status, ok := v.checks[ref]
	v.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"provider_ref": ref, "status": status, "document_type": "national_id", "country": "KE"})
}

// Webhook builds the signed result the vendor would send for ref, with
// tamper applied, without sending it.
func (v *Vendor) Webhook(ref, status string, tamper Tamper) (http.Header, []byte) {
	body, _ := json.Marshal(map[string]any{
		"event_id":     uuid.NewString(),
		"provider_ref": ref,
		"status":       status,
		"summary":      map[string]string{"document_type": "national_id", "country": "KE"},
	})
	ts := time.Now()
	switch tamper {
	case Stale:
		ts = ts.Add(-10 * time.Minute)
	case Future:
		ts = ts.Add(10 * time.Minute)
	}
	stamp := strconv.FormatInt(ts.Unix(), 10)
	secret := v.secret
	if tamper == WrongSecret {
		secret = []byte("not-the-shared-secret")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(stamp + "." + string(body)))
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set(HeaderTimestamp, stamp)
	if tamper != Unsigned {
		h.Set(HeaderSignature, hex.EncodeToString(mac.Sum(nil)))
	}
	if tamper == TamperBody {
		body = bytes.Replace(body, []byte(status), []byte(strings.ToUpper(status)), 1)
	}
	return h, body
}

// Decide records status for ref and posts the signed webhook to the
// configured URL, returning the response status code.
func (v *Vendor) Decide(ref, status string, tamper Tamper) (int, error) {
	v.mu.Lock()
	url := v.webhookURL
	if _, ok := v.checks[ref]; ok {
		v.checks[ref] = status
	}
	v.mu.Unlock()
	if url == "" {
		return 0, fmt.Errorf("kycvendor: no webhook URL set")
	}
	header, body := v.Webhook(ref, status, tamper)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}
