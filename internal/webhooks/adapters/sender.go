// Package adapters holds webhooks' connections to the outside: clients'
// endpoints (Sender), and accounts' and identity's gRPC APIs.
package adapters

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

// Outcome is how one request to an endpoint went, as
// webhooks.delivery_attempts.outcome records it.
type Outcome string

// The outcomes (data model §8).
const (
	OutcomeSucceeded  Outcome = "succeeded"   // a 2xx answer
	OutcomeHTTPStatus Outcome = "http_status" // any other answer, a redirect included
	OutcomeTimeout    Outcome = "timeout"
	OutcomeConnect    Outcome = "connect"
	OutcomeTLS        Outcome = "tls"
	OutcomeRefusedURL Outcome = "refused_url" // not a URL or address VASPRO calls
)

// The headers every request carries besides the caller's.
const (
	ContentType = "application/json"
	UserAgent   = "VASPRO-Webhooks/1"
)

// MaxResponseBody is how much of an answer is kept (the schema's 4 KB).
const MaxResponseBody = 4096

// DefaultTimeout is each request's limit (OQ-23).
const DefaultTimeout = 10 * time.Second

// SenderConfig configures a Sender.
type SenderConfig struct {
	// AllowInsecure allows plain http and private addresses, for
	// development and tests only.
	AllowInsecure bool
	// Timeout limits each request, the answer's body included;
	// DefaultTimeout when zero.
	Timeout time.Duration
	// RootCAs are the trusted roots; the system's when nil.
	RootCAs *x509.CertPool
	// Clock times requests; the real clock when nil.
	Clock clock.Clock
}

// Request is one POST to an endpoint.
type Request struct {
	URL     string
	Headers map[string]string
	Body    []byte
}

// Response is what came back, or why nothing did. Error never names
// the URL, which may carry the client's own tokens.
type Response struct {
	Outcome   Outcome
	Status    int
	Headers   http.Header
	Body      []byte // at most MaxResponseBody bytes
	Truncated bool
	Error     string // set exactly when there's no answer
	Duration  time.Duration
}

var errAddressRefused = errors.New("address not allowed")

// Sender POSTs webhook bodies to clients' endpoints. It never uses a
// proxy, never follows a redirect, and refuses, at the moment of
// connecting, any address domain.AddressAllowed rejects: the check runs
// on the address actually dialled, after DNS, so a name that resolves
// (or later re-resolves) inward is refused too.
type Sender struct {
	client   *http.Client
	insecure bool
	clk      clock.Clock
}

// NewSender builds a Sender.
func NewSender(cfg SenderConfig) *Sender {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	if !cfg.AllowInsecure {
		dialer.Control = refusePrivate
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   cfg.Timeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
		ResponseHeaderTimeout: cfg.Timeout,
	}
	return &Sender{
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		insecure: cfg.AllowInsecure,
		clk:      cfg.Clock,
	}
}

// refusePrivate is the dialer's last check before connecting.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !domain.AddressAllowed(ap.Addr()) {
		return fmt.Errorf("%w: %s", errAddressRefused, address)
	}
	return nil
}

// Send POSTs req.Body as JSON with req.Headers. It never returns an
// error: what went wrong is the Response's Outcome and Error.
func (s *Sender) Send(ctx context.Context, req Request) Response {
	start := s.clk.Now()
	resp := s.send(ctx, req)
	resp.Duration = s.clk.Now().Sub(start)
	return resp
}

func (s *Sender) send(ctx context.Context, req Request) Response {
	if err := domain.CheckURL(req.URL, s.insecure); err != nil {
		return Response{Outcome: OutcomeRefusedURL, Error: err.Error()}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return Response{Outcome: OutcomeRefusedURL, Error: "not a URL VASPRO can call"}
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", ContentType)
	httpReq.Header.Set("User-Agent", UserAgent)

	httpResp, err := s.client.Do(httpReq) //nolint:gosec // G704: the URL is checked above and every dialled address by refusePrivate
	if err != nil {
		return failure(err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(httpResp.Body, MaxResponseBody+1))
	out := Response{Status: httpResp.StatusCode, Headers: httpResp.Header, Outcome: OutcomeHTTPStatus}
	if httpResp.StatusCode >= 200 && httpResp.StatusCode <= 299 {
		out.Outcome = OutcomeSucceeded
	}
	if readErr != nil && isTimeout(readErr) {
		// The answer's status arrived; its body didn't in time. The
		// status decides, with what body there was.
		out.Truncated = len(body) > 0
	}
	if len(body) > MaxResponseBody {
		body, out.Truncated = body[:MaxResponseBody], true
	}
	if len(body) > 0 {
		out.Body = body
	}
	return out
}

// failure classifies a request that got no answer.
func failure(err error) Response {
	inner := err
	var uerr *url.Error
	if errors.As(err, &uerr) {
		inner = uerr.Err // url.Error's text names the URL
	}
	var (
		certInvalid  x509.CertificateInvalidError
		unknownCA    x509.UnknownAuthorityError
		hostname     x509.HostnameError
		certVerify   *tls.CertificateVerificationError
		recordHeader tls.RecordHeaderError
		alert        tls.AlertError
	)
	switch {
	case errors.Is(err, errAddressRefused):
		return Response{Outcome: OutcomeRefusedURL, Error: inner.Error()}
	case isTimeout(err):
		return Response{Outcome: OutcomeTimeout, Error: "no answer in time"}
	case errors.As(err, &certVerify), errors.As(err, &certInvalid), errors.As(err, &unknownCA),
		errors.As(err, &hostname), errors.As(err, &recordHeader), errors.As(err, &alert):
		return Response{Outcome: OutcomeTLS, Error: inner.Error()}
	default:
		return Response{Outcome: OutcomeConnect, Error: inner.Error()}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}
