// Package kycvendor fakes a KYC vendor for tests and local development
// (task 2.6, ADR 0008): a real HTTP server that starts checks, serves
// reports, and, when a test decides a check, posts a signed result
// webhook, so accounts' real adapter and webhook route run unchanged
// against it. Nothing here touches personal data: a "check" is just a
// reference.
//
// The dev environment runs it standalone (cmd/kycvendor), where testers
// decide checks through POST /checks/{id}/decision instead of Decide.
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
	server    *httptest.Server // nil when standalone
	handler   http.Handler
	publicURL string
	secret    []byte

	mu         sync.Mutex
	webhookURL string
	checks     map[string]*check // provider_ref → check
	byRef      map[string]string // our reference → provider_ref
	order      []string          // provider_refs, oldest first
}

type check struct {
	reference string
	kind      string
	status    string
}

// statuses a decision may set, the ones accounts' adapter reads.
var statuses = map[string]bool{"pending": true, "passed": true, "review": true, "failed": true, "expired": true}

// New starts the fake on a test server, signing webhooks with secret.
// Close it when done.
func New(secret []byte) *Vendor {
	v := newVendor(secret)
	v.server = httptest.NewServer(v.handler)
	v.publicURL = v.server.URL
	return v
}

// NewStandalone builds the fake for a process of its own: serve
// Handler, sign with secret, post results to webhookURL, and give
// publicURL in hosted-flow links.
func NewStandalone(secret []byte, publicURL, webhookURL string) *Vendor {
	v := newVendor(secret)
	v.publicURL = strings.TrimSuffix(publicURL, "/")
	v.webhookURL = webhookURL
	return v
}

func newVendor(secret []byte) *Vendor {
	v := &Vendor{secret: secret, checks: map[string]*check{}, byRef: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /checks", v.startCheck)
	mux.HandleFunc("GET /checks", v.listChecks)
	mux.HandleFunc("GET /checks/{ref}/report", v.report)
	mux.HandleFunc("POST /checks/{id}/decision", v.decide)
	v.handler = mux
	return v
}

// Handler serves the vendor's HTTP API.
func (v *Vendor) Handler() http.Handler { return v.handler }

// URL is the vendor's base URL (accounts' ACCOUNTS_KYC_FAKE_URL).
func (v *Vendor) URL() string { return v.publicURL }

// Close stops the test server, if there is one.
func (v *Vendor) Close() {
	if v.server != nil {
		v.server.Close()
	}
}

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
	v.checks[ref] = &check{reference: req.Reference, kind: req.Kind, status: "pending"}
	v.byRef[req.Reference] = ref
	v.order = append(v.order, ref)
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"provider_ref": ref, "url": v.publicURL + "/flow/" + ref})
}

func (v *Vendor) report(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	v.mu.Lock()
	c, ok := v.checks[ref]
	var status string
	if ok {
		status = c.status
	}
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
	if c, ok := v.checks[ref]; ok {
		c.status = status
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

// listChecks shows every check this process has started, oldest first,
// so a tester can find the one to decide.
func (v *Vendor) listChecks(w http.ResponseWriter, _ *http.Request) {
	v.mu.Lock()
	out := make([]map[string]string, 0, len(v.order))
	for _, ref := range v.order {
		c := v.checks[ref]
		out = append(out, map[string]string{"reference": c.reference, "provider_ref": ref, "kind": c.kind, "status": c.status})
	}
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// decide is a tester deciding a check, named by our check ID (the
// reference accounts sent) or by the vendor's provider_ref: it posts
// the signed result webhook and answers with the webhook's status.
func (v *Vendor) decide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !statuses[req.Status] || req.Status == "pending" {
		http.Error(w, "status must be passed, review, failed or expired", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	v.mu.Lock()
	ref, ok := v.byRef[id]
	if !ok {
		if _, known := v.checks[id]; known {
			ref, ok = id, true
		}
	}
	v.mu.Unlock()
	if !ok {
		http.Error(w, "no such check (the fake forgets checks when it restarts)", http.StatusNotFound)
		return
	}
	code, err := v.Decide(ref, req.Status, NoTamper)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if code >= 300 {
		w.WriteHeader(http.StatusBadGateway)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"provider_ref": ref, "status": req.Status, "webhook_status": code})
}
