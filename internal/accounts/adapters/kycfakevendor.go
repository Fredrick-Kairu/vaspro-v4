package adapters

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
)

// webhookWindow is how far a webhook's timestamp may be from now,
// either way, before it's refused as stale or replayed.
const webhookWindow = 5 * time.Minute

// Header names the fake vendor signs its webhooks with
// (tools/fakes/kycvendor).
const (
	fakeVendorTimestampHeader = "X-FakeKYC-Timestamp"
	fakeVendorSignatureHeader = "X-FakeKYC-Signature"
)

// FakeVendorProvider is KYCProvider for the fake vendor
// (tools/fakes/kycvendor), the "fake" provider OD-03's default names
// until a real vendor is chosen (ADR 0008). It speaks HTTP to the
// vendor and verifies webhooks signed HMAC-SHA256(secret,
// timestamp + "." + body), the shape most vendors use.
type FakeVendorProvider struct {
	baseURL string
	secret  []byte
	client  *http.Client
}

// NewFakeVendorProvider talks to the fake vendor at baseURL and checks
// webhooks against secret.
func NewFakeVendorProvider(baseURL string, secret []byte) *FakeVendorProvider {
	return &FakeVendorProvider{baseURL: baseURL, secret: secret, client: &http.Client{Timeout: 10 * time.Second}}
}

func (p *FakeVendorProvider) Name() string { return "fake" }

func (p *FakeVendorProvider) StartCheck(ctx context.Context, req KYCStart) (KYCStarted, error) {
	body, err := json.Marshal(map[string]string{"reference": req.CheckID.String(), "kind": req.Kind, "subject_id": req.SubjectID.String()})
	if err != nil {
		return KYCStarted{}, fmt.Errorf("adapters: encoding KYC start: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/checks", bytes.NewReader(body))
	if err != nil {
		return KYCStarted{}, fmt.Errorf("adapters: building KYC start: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	var out struct {
		ProviderRef string `json:"provider_ref"`
		URL         string `json:"url"`
	}
	if err := p.do(httpReq, &out); err != nil {
		return KYCStarted{}, fmt.Errorf("adapters: starting KYC check: %w", err)
	}
	if out.ProviderRef == "" {
		return KYCStarted{}, fmt.Errorf("adapters: the KYC vendor returned no reference")
	}
	return KYCStarted{ProviderRef: out.ProviderRef, URL: out.URL}, nil
}

func (p *FakeVendorProvider) ParseWebhook(header http.Header, body []byte, now time.Time) (KYCResult, error) {
	stamp := header.Get(fakeVendorTimestampHeader)
	unix, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return KYCResult{}, ErrBadWebhookSignature
	}
	if d := now.Sub(time.Unix(unix, 0)); d > webhookWindow || d < -webhookWindow {
		return KYCResult{}, ErrBadWebhookSignature
	}
	sig, err := hex.DecodeString(header.Get(fakeVendorSignatureHeader))
	if err != nil || len(sig) == 0 || !vascrypto.HMACVerify(p.secret, []byte(stamp+"."+string(body)), sig) {
		return KYCResult{}, ErrBadWebhookSignature
	}

	var in struct {
		ProviderRef string         `json:"provider_ref"`
		Status      string         `json:"status"`
		Summary     map[string]any `json:"summary"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return KYCResult{}, fmt.Errorf("adapters: decoding KYC webhook: %w", err)
	}
	switch in.Status {
	case "passed", "review", "failed", "expired":
	default:
		return KYCResult{}, fmt.Errorf("adapters: unknown KYC status %q", in.Status)
	}
	if in.ProviderRef == "" {
		return KYCResult{}, fmt.Errorf("adapters: KYC webhook without a reference")
	}
	return KYCResult{ProviderRef: in.ProviderRef, Status: in.Status, Summary: in.Summary}, nil
}

func (p *FakeVendorProvider) FetchReport(ctx context.Context, providerRef string) (map[string]any, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/checks/"+url.PathEscape(providerRef)+"/report", nil)
	if err != nil {
		return nil, fmt.Errorf("adapters: building KYC report request: %w", err)
	}
	var out map[string]any
	if err := p.do(httpReq, &out); err != nil {
		return nil, fmt.Errorf("adapters: fetching KYC report: %w", err)
	}
	return out, nil
}

func (p *FakeVendorProvider) do(req *http.Request, out any) error {
	// The URL is the operator's ACCOUNTS_KYC_FAKE_URL (or a real vendor's
	// fixed endpoint), never anything a client sends.
	resp, err := p.client.Do(req) //nolint:gosec // G704: the base URL is operator configuration, not request input
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vendor answered %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
