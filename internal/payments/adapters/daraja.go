// Package adapters holds payments' outbound integrations, each behind
// an interface with a fake in tools/fakes (task 4.2: Daraja).
package adapters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// DarajaCredentials are one rail account's Daraja app and shortcode.
type DarajaCredentials struct {
	ConsumerKey    string
	ConsumerSecret string
	ShortCode      string
	Passkey        string
}

// CredentialSource decrypts a rail account's credentials at call time
// (data model §7: decrypted only inside the rail adapter).
type CredentialSource interface {
	DarajaCredentials(ctx context.Context, railAccountID uuid.UUID) (DarajaCredentials, error)
}

// DarajaConfig configures the Daraja client.
type DarajaConfig struct {
	BaseURL     string
	Cache       *cache.Cache
	Credentials CredentialSource
	// Location is the country pack's time zone, which Daraja's
	// timestamps are in (Africa/Nairobi for KE). Required.
	Location   *time.Location
	Clock      clock.Clock
	HTTPClient *http.Client // default: a 30s timeout
}

// Daraja is Safaricom's Daraja API: OAuth, STK push and query, and C2B
// URL registration. It logs nothing; its errors carry no credential,
// token or payer data.
type Daraja struct {
	cfg    DarajaConfig
	flight singleflight.Group
}

// NewDaraja builds the client. A missing Location is a wiring bug.
func NewDaraja(cfg DarajaConfig) *Daraja {
	if cfg.Location == nil {
		panic("adapters: DarajaConfig.Location is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	return &Daraja{cfg: cfg}
}

// Errors a Daraja call can end in.
var (
	ErrRailTimeout     = errors.New("adapters: the rail didn't answer in time")
	ErrRailUnavailable = errors.New("adapters: the rail couldn't be reached")
	ErrRailRefused     = errors.New("adapters: the rail refused the request")
	ErrBadCallback     = errors.New("adapters: malformed callback")
	ErrInvalidRequest  = errors.New("adapters: invalid request")
)

// RailError is an error body Daraja answered with.
type RailError struct {
	Status  int
	Code    string // Daraja's errorCode, e.g. 400.002.02
	Message string
}

func (e *RailError) Error() string {
	return fmt.Sprintf("adapters: Daraja answered %d %s: %s", e.Status, e.Code, e.Message)
}

func (e *RailError) Unwrap() error {
	if e.Status >= 500 {
		return ErrRailUnavailable
	}
	return ErrRailRefused
}

// Daraja's own error codes this client acts on.
const (
	errCodeInvalidToken = "404.001.03"
	errCodeProcessing   = "500.001.1001"
)

// tokenSkew is how long before its expiry a cached token is dropped.
const tokenSkew = 60 * time.Second

// ErrorCodeFor maps an adapter error to the catalogue code an API
// answers with.
func ErrorCodeFor(err error) problem.Code {
	switch {
	case errors.Is(err, ErrRailTimeout):
		return problem.PAY6502
	case errors.Is(err, ErrRailUnavailable), errors.Is(err, ErrRailRefused):
		return problem.PAY6507
	case errors.Is(err, ErrInvalidRequest):
		return problem.VAL4000
	default:
		return problem.SYS9000
	}
}

// STKPushRequest asks a payer's phone to pay a paybill.
type STKPushRequest struct {
	RailAccountID    uuid.UUID
	AmountMinor      int64 // whole shillings only
	PhoneE164        string
	AccountReference string // at most 12 characters
	Description      string // at most 13 characters; "Payment" if empty
	CallbackURL      string
}

// STKPushResponse is Daraja's acceptance of a push.
type STKPushResponse struct {
	MerchantRequestID string
	CheckoutRequestID string
}

// STKResult is an STK push's outcome, from its callback or a query. A
// query doesn't report the receipt, amount, phone or time.
type STKResult struct {
	MerchantRequestID string
	CheckoutRequestID string
	Pending           bool
	ResultCode        int
	ResultDesc        string
	Receipt           string
	AmountMinor       int64
	PhoneE164         string
	PaidAt            time.Time
}

// Succeeded reports a final, successful payment.
func (r STKResult) Succeeded() bool { return !r.Pending && r.ResultCode == 0 }

// ErrorCode is the PAY code for a failed result, empty otherwise.
func (r STKResult) ErrorCode() problem.Code {
	if r.Pending {
		return ""
	}
	switch r.ResultCode {
	case 0:
		return ""
	case 1032:
		return problem.PAY6503
	case 1037:
		return problem.PAY6504
	case 1:
		return problem.PAY6505
	case 2001:
		return problem.PAY6506
	default:
		return problem.PAY6507
	}
}

var (
	msisdnRe      = regexp.MustCompile(`^254[17]\d{8}$`)
	shillingsRe   = regexp.MustCompile(`^\d{1,15}(\.\d{1,2})?$`)
	darajaTimeFmt = "20060102150405"
)

// STKPush prompts the payer's phone. The callback reports the outcome.
func (d *Daraja) STKPush(ctx context.Context, req STKPushRequest) (STKPushResponse, error) {
	msisdn := strings.TrimPrefix(req.PhoneE164, "+")
	desc := req.Description
	if desc == "" {
		desc = "Payment"
	}
	switch {
	case req.AmountMinor <= 0 || req.AmountMinor%100 != 0:
		return STKPushResponse{}, fmt.Errorf("%w: STK amounts are whole shillings", ErrInvalidRequest)
	case !strings.HasPrefix(req.PhoneE164, "+") || !msisdnRe.MatchString(msisdn):
		return STKPushResponse{}, fmt.Errorf("%w: not a Kenyan mobile number", ErrInvalidRequest)
	case req.AccountReference == "" || len(req.AccountReference) > 12:
		return STKPushResponse{}, fmt.Errorf("%w: the account reference must be 1 to 12 characters", ErrInvalidRequest)
	case len(desc) > 13:
		return STKPushResponse{}, fmt.Errorf("%w: the description must be at most 13 characters", ErrInvalidRequest)
	case req.CallbackURL == "":
		return STKPushResponse{}, fmt.Errorf("%w: a callback URL is required", ErrInvalidRequest)
	}
	creds, err := d.credentials(ctx, req.RailAccountID)
	if err != nil {
		return STKPushResponse{}, err
	}
	ts, password := d.password(creds)
	body := map[string]any{
		"BusinessShortCode": creds.ShortCode,
		"Password":          password,
		"Timestamp":         ts,
		"TransactionType":   "CustomerPayBillOnline",
		"Amount":            req.AmountMinor / 100,
		"PartyA":            msisdn,
		"PartyB":            creds.ShortCode,
		"PhoneNumber":       msisdn,
		"CallBackURL":       req.CallbackURL,
		"AccountReference":  req.AccountReference,
		"TransactionDesc":   desc,
	}
	var out struct {
		MerchantRequestID string `json:"MerchantRequestID"`
		CheckoutRequestID string `json:"CheckoutRequestID"`
		ResponseCode      string `json:"ResponseCode"`
	}
	if err := d.call(ctx, req.RailAccountID, creds, "/mpesa/stkpush/v1/processrequest", body, &out); err != nil {
		return STKPushResponse{}, fmt.Errorf("adapters: STK push: %w", err)
	}
	if out.ResponseCode != "0" || out.CheckoutRequestID == "" {
		return STKPushResponse{}, fmt.Errorf("adapters: STK push: %w: response code %q", ErrRailRefused, out.ResponseCode)
	}
	return STKPushResponse{MerchantRequestID: out.MerchantRequestID, CheckoutRequestID: out.CheckoutRequestID}, nil
}

// STKQuery asks for a push's outcome; Pending until the payer acts.
func (d *Daraja) STKQuery(ctx context.Context, railAccountID uuid.UUID, checkoutRequestID string) (STKResult, error) {
	creds, err := d.credentials(ctx, railAccountID)
	if err != nil {
		return STKResult{}, err
	}
	ts, password := d.password(creds)
	body := map[string]string{
		"BusinessShortCode": creds.ShortCode,
		"Password":          password,
		"Timestamp":         ts,
		"CheckoutRequestID": checkoutRequestID,
	}
	var out struct {
		MerchantRequestID string      `json:"MerchantRequestID"`
		CheckoutRequestID string      `json:"CheckoutRequestID"`
		ResultCode        json.Number `json:"ResultCode"`
		ResultDesc        string      `json:"ResultDesc"`
	}
	err = d.call(ctx, railAccountID, creds, "/mpesa/stkpushquery/v1/query", body, &out)
	var re *RailError
	if errors.As(err, &re) && re.Code == errCodeProcessing {
		return STKResult{CheckoutRequestID: checkoutRequestID, Pending: true}, nil
	}
	if err != nil {
		return STKResult{}, fmt.Errorf("adapters: STK query: %w", err)
	}
	code, err := strconv.Atoi(out.ResultCode.String())
	if err != nil {
		return STKResult{}, fmt.Errorf("adapters: STK query: %w: no result code", ErrRailRefused)
	}
	return STKResult{
		MerchantRequestID: out.MerchantRequestID, CheckoutRequestID: out.CheckoutRequestID,
		ResultCode: code, ResultDesc: out.ResultDesc,
	}, nil
}

// RegisterC2BURLs registers a paybill's confirmation and validation
// URLs. ResponseType is Completed: if validation can't be reached, the
// payment goes through, and an unknown account number goes to suspense
// (OQ-22).
func (d *Daraja) RegisterC2BURLs(ctx context.Context, railAccountID uuid.UUID, confirmationURL, validationURL string) error {
	creds, err := d.credentials(ctx, railAccountID)
	if err != nil {
		return err
	}
	body := map[string]string{
		"ShortCode":       creds.ShortCode,
		"ResponseType":    "Completed",
		"ConfirmationURL": confirmationURL,
		"ValidationURL":   validationURL,
	}
	var out struct {
		ResponseCode string `json:"ResponseCode"`
	}
	if err := d.call(ctx, railAccountID, creds, "/mpesa/c2b/v1/registerurl", body, &out); err != nil {
		return fmt.Errorf("adapters: registering C2B URLs: %w", err)
	}
	if out.ResponseCode != "0" {
		return fmt.Errorf("adapters: registering C2B URLs: %w: response code %q", ErrRailRefused, out.ResponseCode)
	}
	return nil
}

func (d *Daraja) credentials(ctx context.Context, railAccountID uuid.UUID) (DarajaCredentials, error) {
	creds, err := d.cfg.Credentials.DarajaCredentials(ctx, railAccountID)
	if err != nil {
		return DarajaCredentials{}, fmt.Errorf("adapters: reading Daraja credentials: %w", err)
	}
	return creds, nil
}

// password is Daraja's Timestamp and Password, base64(shortcode +
// passkey + timestamp), in the pack's time zone.
func (d *Daraja) password(creds DarajaCredentials) (string, string) {
	ts := d.cfg.Clock.Now().In(d.cfg.Location).Format(darajaTimeFmt)
	return ts, base64.StdEncoding.EncodeToString([]byte(creds.ShortCode + creds.Passkey + ts))
}

// call posts body with a bearer token. A refused token is dropped from
// the cache and the call retried once with a fresh one.
func (d *Daraja) call(ctx context.Context, railAccountID uuid.UUID, creds DarajaCredentials, path string, body, out any) error {
	key := tokenKey(railAccountID, creds)
	for attempt := 0; ; attempt++ {
		token, err := d.token(ctx, key, creds)
		if err != nil {
			return err
		}
		err = d.do(ctx, http.MethodPost, path, body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }, out)
		var re *RailError
		if errors.As(err, &re) && (re.Status == http.StatusUnauthorized || re.Code == errCodeInvalidToken) {
			d.dropToken(ctx, key, token)
			if attempt == 0 {
				continue
			}
		}
		return err
	}
}

// tokenKey names a cached token by rail account and a fingerprint of
// the consumer key, so new credentials never reuse an old token.
func tokenKey(railAccountID uuid.UUID, creds DarajaCredentials) string {
	sum := sha256.Sum256([]byte(creds.ConsumerKey))
	return "payments:daraja:token:" + railAccountID.String() + ":" + hex.EncodeToString(sum[:8])
}

// token returns a cached token, or fetches one. Concurrent callers in
// this process share one fetch. Redis being down costs a fetch per
// call, not the payment.
func (d *Daraja) token(ctx context.Context, key string, creds DarajaCredentials) (string, error) {
	if t, err := d.cfg.Cache.Get(ctx, key).Result(); err == nil {
		return t, nil
	}
	v, err, _ := d.flight.Do(key, func() (any, error) {
		if t, err := d.cfg.Cache.Get(ctx, key).Result(); err == nil {
			return t, nil
		}
		token, ttl, err := d.fetchToken(ctx, creds)
		if err != nil {
			return "", err
		}
		if ttl > 0 {
			_ = d.cfg.Cache.Set(ctx, key, token, ttl).Err()
		}
		return token, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (d *Daraja) fetchToken(ctx context.Context, creds DarajaCredentials) (string, time.Duration, error) {
	var out struct {
		AccessToken string      `json:"access_token"`
		ExpiresIn   json.Number `json:"expires_in"`
	}
	err := d.do(ctx, http.MethodGet, "/oauth/v1/generate?grant_type=client_credentials", nil,
		func(r *http.Request) { r.SetBasicAuth(creds.ConsumerKey, creds.ConsumerSecret) }, &out)
	if err != nil {
		return "", 0, fmt.Errorf("adapters: Daraja OAuth: %w", err)
	}
	secs, err := strconv.Atoi(out.ExpiresIn.String())
	if out.AccessToken == "" || err != nil {
		return "", 0, fmt.Errorf("adapters: Daraja OAuth: %w: no token in the response", ErrRailRefused)
	}
	return out.AccessToken, time.Duration(secs)*time.Second - tokenSkew, nil
}

// dropScript deletes the key only if it still holds the refused token,
// so a fresh token another caller cached survives.
var dropScript = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`)

func (d *Daraja) dropToken(ctx context.Context, key, token string) {
	_ = dropScript.Run(ctx, d.cfg.Cache, []string{key}, token).Err()
}

// do sends one request and decodes a 2xx body into out, or returns a
// *RailError, ErrRailTimeout or ErrRailUnavailable.
func (d *Daraja) do(ctx context.Context, method, path string, body any, auth func(*http.Request), out any) error {
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("adapters: encoding Daraja request: %w", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.cfg.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("adapters: building Daraja request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	auth(req)
	resp, err := d.cfg.HTTPClient.Do(req) //nolint:gosec // G704: the base URL is operator configuration, not request input
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return ErrRailTimeout
		}
		return ErrRailUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			ErrorCode    string `json:"errorCode"`
			ErrorMessage string `json:"errorMessage"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return &RailError{Status: resp.StatusCode, Code: e.ErrorCode, Message: e.ErrorMessage}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("adapters: decoding Daraja response: %w", ErrRailUnavailable)
	}
	return nil
}

// ParseSTKCallback reads an STK result callback. Whether it came from
// Daraja is the caller's check (source and path token, OQ-22).
func ParseSTKCallback(body []byte, loc *time.Location) (STKResult, error) {
	var in struct {
		Body struct {
			Callback *struct {
				MerchantRequestID string       `json:"MerchantRequestID"`
				CheckoutRequestID string       `json:"CheckoutRequestID"`
				ResultCode        *json.Number `json:"ResultCode"`
				ResultDesc        string       `json:"ResultDesc"`
				Metadata          *struct {
					Item []struct {
						Name  string          `json:"Name"`
						Value json.RawMessage `json:"Value"`
					} `json:"Item"`
				} `json:"CallbackMetadata"`
			} `json:"stkCallback"`
		} `json:"Body"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return STKResult{}, fmt.Errorf("%w: %w", ErrBadCallback, err)
	}
	cb := in.Body.Callback
	if cb == nil || cb.CheckoutRequestID == "" || cb.ResultCode == nil {
		return STKResult{}, fmt.Errorf("%w: no checkout ID or result code", ErrBadCallback)
	}
	code, err := strconv.Atoi(cb.ResultCode.String())
	if err != nil {
		return STKResult{}, fmt.Errorf("%w: result code %q", ErrBadCallback, cb.ResultCode.String())
	}
	r := STKResult{MerchantRequestID: cb.MerchantRequestID, CheckoutRequestID: cb.CheckoutRequestID, ResultCode: code, ResultDesc: cb.ResultDesc}
	if code != 0 {
		return r, nil
	}
	if cb.Metadata == nil {
		return STKResult{}, fmt.Errorf("%w: success without metadata", ErrBadCallback)
	}
	for _, it := range cb.Metadata.Item {
		v := strings.Trim(string(it.Value), `"`)
		switch it.Name {
		case "Amount":
			if r.AmountMinor, err = parseMinor(v); err != nil {
				return STKResult{}, err
			}
		case "MpesaReceiptNumber":
			r.Receipt = v
		case "TransactionDate":
			if r.PaidAt, err = parseDarajaTime(v, loc); err != nil {
				return STKResult{}, err
			}
		case "PhoneNumber":
			r.PhoneE164 = phoneE164(v)
		}
	}
	if r.Receipt == "" || r.AmountMinor <= 0 || r.PaidAt.IsZero() {
		return STKResult{}, fmt.Errorf("%w: success without a receipt, amount or date", ErrBadCallback)
	}
	return r, nil
}

// C2BPayment is a validation or confirmation call on a paybill.
type C2BPayment struct {
	TransactionType string
	Receipt         string
	PaidAt          time.Time
	AmountMinor     int64
	ShortCode       string
	BillRef         string // as typed, less surrounding spaces
	OrgBalanceMinor *int64
	PhoneE164       string // empty when Daraja masks the MSISDN
	PayerName       string
}

// flexString accepts a JSON string or number.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// ParseC2BCallback reads a C2B validation or confirmation call.
func ParseC2BCallback(body []byte, loc *time.Location) (C2BPayment, error) {
	var in struct {
		TransactionType   flexString `json:"TransactionType"`
		TransID           flexString `json:"TransID"`
		TransTime         flexString `json:"TransTime"`
		TransAmount       flexString `json:"TransAmount"`
		BusinessShortCode flexString `json:"BusinessShortCode"`
		BillRefNumber     flexString `json:"BillRefNumber"`
		OrgAccountBalance flexString `json:"OrgAccountBalance"`
		MSISDN            flexString `json:"MSISDN"`
		FirstName         flexString `json:"FirstName"`
		MiddleName        flexString `json:"MiddleName"`
		LastName          flexString `json:"LastName"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return C2BPayment{}, fmt.Errorf("%w: %w", ErrBadCallback, err)
	}
	if in.TransID == "" || in.BusinessShortCode == "" {
		return C2BPayment{}, fmt.Errorf("%w: no transaction ID or shortcode", ErrBadCallback)
	}
	amount, err := parseMinor(string(in.TransAmount))
	if err != nil {
		return C2BPayment{}, err
	}
	if amount <= 0 {
		return C2BPayment{}, fmt.Errorf("%w: amount must be positive", ErrBadCallback)
	}
	paidAt, err := parseDarajaTime(string(in.TransTime), loc)
	if err != nil {
		return C2BPayment{}, err
	}
	p := C2BPayment{
		TransactionType: string(in.TransactionType),
		Receipt:         string(in.TransID),
		PaidAt:          paidAt,
		AmountMinor:     amount,
		ShortCode:       string(in.BusinessShortCode),
		BillRef:         strings.TrimSpace(string(in.BillRefNumber)),
		PhoneE164:       phoneE164(string(in.MSISDN)),
	}
	if in.OrgAccountBalance != "" {
		bal, err := parseMinor(string(in.OrgAccountBalance))
		if err != nil {
			return C2BPayment{}, err
		}
		p.OrgBalanceMinor = &bal
	}
	var names []string
	for _, n := range []flexString{in.FirstName, in.MiddleName, in.LastName} {
		if s := strings.TrimSpace(string(n)); s != "" {
			names = append(names, s)
		}
	}
	p.PayerName = strings.Join(names, " ")
	return p, nil
}

// C2BResponse is the body a validation or confirmation call is answered
// with: accepted, or rejected with one of Daraja's C2B000xx codes.
func C2BResponse(accept bool, code string) []byte {
	out := map[string]string{"ResultCode": "0", "ResultDesc": "Accepted"}
	if !accept {
		out = map[string]string{"ResultCode": code, "ResultDesc": "Rejected"}
	}
	b, _ := json.Marshal(out)
	return b
}

// parseMinor reads shillings ("1500", "1500.5", "1500.00") as minor
// units. M-Pesa amounts have at most two decimals.
func parseMinor(s string) (int64, error) {
	if !shillingsRe.MatchString(s) {
		return 0, fmt.Errorf("%w: amount %q", ErrBadCallback, s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	frac = (frac + "00")[:2]
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrBadCallback, s)
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	return w*100 + f, nil
}

func parseDarajaTime(s string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation(darajaTimeFmt, s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: time %q", ErrBadCallback, s)
	}
	return t.UTC(), nil
}

// phoneE164 turns a Daraja MSISDN into E.164, or "" if it isn't one
// (Daraja masks C2B MSISDNs on some accounts).
func phoneE164(s string) string {
	if msisdnRe.MatchString(s) {
		return "+" + s
	}
	return ""
}

// CheckCredentials proves a rail account's consumer key and secret with
// a fresh OAuth call, never a cached token (task 4.3: an account turns
// active only after a test call works). The token isn't cached. Daraja
// has no harmless call that proves the passkey; the first STK push does.
func (d *Daraja) CheckCredentials(ctx context.Context, railAccountID uuid.UUID) error {
	creds, err := d.credentials(ctx, railAccountID)
	if err != nil {
		return err
	}
	if _, _, err := d.fetchToken(ctx, creds); err != nil {
		return err
	}
	return nil
}
