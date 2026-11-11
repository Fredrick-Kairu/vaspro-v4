// Package daraja fakes Safaricom's Daraja API for tests and local
// development (task 4.2): a real HTTP server with OAuth, STK push, STK
// query and C2B URL registration, which posts STK result callbacks and
// C2B validation and confirmation calls to the URLs it's given, so
// payments' real adapter and callback routes run unchanged against it.
//
// The wire shapes follow Daraja's published API. Each STK push ends in
// an Outcome the test scripts (per phone number, or for the next push):
// success, cancelled by the user, timeout, insufficient funds or wrong
// PIN, with the callback delivered normally, twice, before the push's
// own response returns, never, or only when the test releases it.
//
// Statement renders what the fake processed as the M-Pesa org portal's
// statement CSV (columns assumed until an anonymised export confirms
// them, OQ-22), for reconciliation tests.
//
// The only phone number fixtures should use is SandboxMSISDN,
// Safaricom's published sandbox test number. Nothing here is real data.
package daraja

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SandboxMSISDN is Safaricom's published sandbox test number.
const SandboxMSISDN = "254708374149"

// Result codes Daraja reports on an STK push's outcome.
const (
	ResultSuccess           = 0
	ResultInsufficientFunds = 1
	ResultCancelled         = 1032
	ResultTimeout           = 1037
	ResultWrongPIN          = 2001
)

// resultDescs are Daraja's own texts for each result code.
var resultDescs = map[int]string{
	ResultSuccess:           "The service request is processed successfully.",
	ResultInsufficientFunds: "The balance is insufficient for the transaction.",
	ResultCancelled:         "Request cancelled by user",
	ResultTimeout:           "DS timeout user cannot be reached",
	ResultWrongPIN:          "The initiator information is invalid.",
}

// Error codes Daraja returns in its error bodies.
const (
	ErrCodeInvalidToken = "404.001.03"
	ErrCodeBadRequest   = "400.002.02"
	ErrCodeProcessing   = "500.001.1001"
)

// Delivery says how an STK result callback reaches the callback URL.
type Delivery int

const (
	Deliver               Delivery = iota // once, after the push's response
	DeliverTwice                          // the same callback twice
	DeliverBeforeResponse                 // once, before the push's response returns
	NoCallback                            // never; only STK query learns the result
	Manual                                // pending until the test calls Release
)

// Outcome is how one STK push ends.
type Outcome struct {
	ResultCode int
	Delivery   Delivery
	// PaidMinor, when set, is the amount the callback reports instead of
	// the amount requested (a successful push only).
	PaidMinor int64
}

// App is a Daraja app and the shortcode it may charge to.
type App struct {
	ConsumerKey    string
	ConsumerSecret string
	ShortCode      string
	Passkey        string
}

// CallbackResult is one callback the fake sent and what came back.
type CallbackResult struct {
	CheckoutRequestID string
	Status            int
	Err               error
}

// Txn is money the fake moved into a shortcode.
type Txn struct {
	Receipt     string
	ShortCode   string
	Kind        string // stk or c2b
	At          time.Time
	AmountMinor int64
	Phone       string
	BillRef     string
}

// EAT is Kenya's time zone (UTC+3, no daylight saving), which Daraja's
// timestamps and statements use.
var EAT = time.FixedZone("EAT", 3*60*60)

type stk struct {
	merchantID, checkoutID string
	shortCode, phone       string
	amountMinor            int64
	accountRef             string
	callbackURL            string
	outcome                Outcome
	final                  bool
	receipt                string
	completedAt            time.Time
}

type c2bURLs struct {
	confirmation, validation, responseType string
}

// Fake is the fake Daraja server.
type Fake struct {
	server *httptest.Server
	client *http.Client

	mu          sync.Mutex
	now         func() time.Time
	apps        map[string]App    // by consumer key
	passkeys    map[string]string // shortcode → passkey
	tokens      map[string]time.Time
	tokenTTL    time.Duration
	tokenIssues int
	scripts     map[string]Outcome
	next        []Outcome
	stks        map[string]*stk
	c2b         map[string]c2bURLs
	txns        []Txn
	callbacks   []CallbackResult
	pushes      int
	seq         int
	wg          sync.WaitGroup
}

// New starts the fake with apps registered. Close it when done.
func New(apps ...App) *Fake {
	f := &Fake{
		client:   &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
		apps:     map[string]App{},
		passkeys: map[string]string{},
		tokens:   map[string]time.Time{},
		tokenTTL: 3599 * time.Second,
		scripts:  map[string]Outcome{},
		stks:     map[string]*stk{},
		c2b:      map[string]c2bURLs{},
	}
	for _, a := range apps {
		f.apps[a.ConsumerKey] = a
		f.passkeys[a.ShortCode] = a.Passkey
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/v1/generate", f.generateToken)
	mux.HandleFunc("POST /mpesa/stkpush/v1/processrequest", f.stkPush)
	mux.HandleFunc("POST /mpesa/stkpushquery/v1/query", f.stkQuery)
	mux.HandleFunc("POST /mpesa/c2b/v1/registerurl", f.registerURL)
	f.server = httptest.NewServer(mux)
	return f
}

// URL is the fake's base URL (payments' DARAJA_BASE_URL).
func (f *Fake) URL() string { return f.server.URL }

// Close waits for callbacks in flight, then stops the server.
func (f *Fake) Close() {
	f.wg.Wait()
	f.server.Close()
}

// SetNow replaces the fake's clock.
func (f *Fake) SetNow(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// SetTokenTTL sets the lifetime of tokens issued from now on.
func (f *Fake) SetTokenTTL(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenTTL = d
}

// ExpireTokens makes every token issued so far invalid at once.
func (f *Fake) ExpireTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.tokens)
}

// TokenIssues is how many tokens the fake has issued.
func (f *Fake) TokenIssues() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenIssues
}

// Script sets the outcome of every STK push to phone (2547XXXXXXXX).
func (f *Fake) Script(phone string, o Outcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[phone] = o
}

// Next queues outcomes for the next STK pushes, whatever their phone.
// A queued outcome wins over a phone's script.
func (f *Fake) Next(o ...Outcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = append(f.next, o...)
}

// Wait blocks until every callback sent in the background has been
// delivered.
func (f *Fake) Wait() { f.wg.Wait() }

// Pushes counts the STK pushes the fake accepted.
func (f *Fake) Pushes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushes
}

// Callbacks lists the STK callbacks sent so far, in order.
func (f *Fake) Callbacks() []CallbackResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.callbacks)
}

// Transactions lists the money the fake has moved, in order.
func (f *Fake) Transactions() []Txn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.txns)
}

// Release completes a Manual push with its scripted result and sends
// its callback, returning the status the callback URL answered.
func (f *Fake) Release(checkoutRequestID string) (int, error) {
	f.mu.Lock()
	s, ok := f.stks[checkoutRequestID]
	if !ok || s.final {
		f.mu.Unlock()
		return 0, fmt.Errorf("daraja fake: no pending push %q", checkoutRequestID)
	}
	f.finalizeLocked(s)
	f.mu.Unlock()
	return f.sendCallback(s)
}

func (f *Fake) generateToken(w http.ResponseWriter, r *http.Request) {
	key, secret, ok := r.BasicAuth()
	f.mu.Lock()
	app, known := f.apps[key]
	f.mu.Unlock()
	if r.URL.Query().Get("grant_type") != "client_credentials" {
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Invalid grant type passed")
		return
	}
	if !ok || !known || app.ConsumerSecret != secret {
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Invalid Authentication passed")
		return
	}
	token := randomHex(14)
	f.mu.Lock()
	f.tokens[token] = f.now().Add(f.tokenTTL)
	f.tokenIssues++
	ttl := f.tokenTTL
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{
		"access_token": token,
		"expires_in":   strconv.Itoa(int(ttl / time.Second)),
	})
}

// authorized checks the bearer token, answering 401 if it isn't valid.
func (f *Fake) authorized(w http.ResponseWriter, r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	expiry, known := f.tokens[token]
	valid := ok && known && f.now().Before(expiry)
	f.mu.Unlock()
	if !valid {
		writeError(w, http.StatusUnauthorized, ErrCodeInvalidToken, "Invalid Access Token")
	}
	return valid
}

var (
	timestampRe = regexp.MustCompile(`^\d{14}$`)
	msisdnRe    = regexp.MustCompile(`^254[17]\d{8}$`)
)

// checkPassword checks Password = base64(shortcode + passkey + timestamp).
func (f *Fake) checkPassword(shortCode, password, timestamp string) bool {
	f.mu.Lock()
	passkey, ok := f.passkeys[shortCode]
	f.mu.Unlock()
	if !ok || !timestampRe.MatchString(timestamp) {
		return false
	}
	return password == base64.StdEncoding.EncodeToString([]byte(shortCode+passkey+timestamp))
}

type stkPushRequest struct {
	BusinessShortCode string      `json:"BusinessShortCode"`
	Password          string      `json:"Password"`
	Timestamp         string      `json:"Timestamp"`
	TransactionType   string      `json:"TransactionType"`
	Amount            json.Number `json:"Amount"`
	PartyA            string      `json:"PartyA"`
	PartyB            string      `json:"PartyB"`
	PhoneNumber       string      `json:"PhoneNumber"`
	CallBackURL       string      `json:"CallBackURL"`
	AccountReference  string      `json:"AccountReference"`
	TransactionDesc   string      `json:"TransactionDesc"`
}

func (f *Fake) stkPush(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
		return
	}
	var req stkPushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - Invalid JSON")
		return
	}
	if msg := validatePush(req); msg != "" {
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - "+msg)
		return
	}
	if !f.checkPassword(req.BusinessShortCode, req.Password, req.Timestamp) {
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - Invalid Password")
		return
	}
	shillings, _ := strconv.ParseInt(req.Amount.String(), 10, 64)

	f.mu.Lock()
	outcome, scripted := f.scripts[req.PhoneNumber]
	if len(f.next) > 0 {
		outcome, f.next = f.next[0], f.next[1:]
	} else if !scripted {
		outcome = Outcome{ResultCode: ResultSuccess}
	}
	f.seq++
	f.pushes++
	s := &stk{
		merchantID:  fmt.Sprintf("fake-%d-%d", f.now().Unix(), f.seq),
		checkoutID:  fmt.Sprintf("ws_CO_%s%06d", f.now().In(EAT).Format("02012006150405"), f.seq),
		shortCode:   req.BusinessShortCode,
		phone:       req.PhoneNumber,
		amountMinor: shillings * 100,
		accountRef:  req.AccountReference,
		callbackURL: req.CallBackURL,
		outcome:     outcome,
	}
	f.stks[s.checkoutID] = s
	if outcome.Delivery == DeliverBeforeResponse || outcome.Delivery == NoCallback {
		f.finalizeLocked(s)
	}
	f.mu.Unlock()

	switch outcome.Delivery {
	case DeliverBeforeResponse:
		_, _ = f.sendCallback(s)
	case Deliver, DeliverTwice:
		f.wg.Go(func() {
			f.mu.Lock()
			f.finalizeLocked(s)
			f.mu.Unlock()
			_, _ = f.sendCallback(s)
			if outcome.Delivery == DeliverTwice {
				_, _ = f.sendCallback(s)
			}
		})
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"MerchantRequestID":   s.merchantID,
		"CheckoutRequestID":   s.checkoutID,
		"ResponseCode":        "0",
		"ResponseDescription": "Success. Request accepted for processing",
		"CustomerMessage":     "Success. Request accepted for processing",
	})
}

func validatePush(req stkPushRequest) string {
	switch {
	case req.TransactionType != "CustomerPayBillOnline" && req.TransactionType != "CustomerBuyGoodsOnline":
		return "Invalid TransactionType"
	case !msisdnRe.MatchString(req.PhoneNumber) || req.PartyA != req.PhoneNumber:
		return "Invalid PhoneNumber"
	case req.CallBackURL == "":
		return "Invalid CallBackURL"
	case req.AccountReference == "" || len(req.AccountReference) > 12:
		return "Invalid AccountReference"
	case len(req.TransactionDesc) > 13:
		return "Invalid TransactionDesc"
	}
	amount, err := strconv.ParseInt(req.Amount.String(), 10, 64)
	if err != nil || amount < 1 {
		return "Invalid Amount"
	}
	return ""
}

// finalizeLocked fixes s's result; f.mu must be held.
func (f *Fake) finalizeLocked(s *stk) {
	if s.final {
		return
	}
	s.final = true
	s.completedAt = f.now()
	if s.outcome.ResultCode != ResultSuccess {
		return
	}
	s.receipt = newReceipt()
	f.txns = append(f.txns, Txn{
		Receipt: s.receipt, ShortCode: s.shortCode, Kind: "stk", At: s.completedAt,
		AmountMinor: s.paidMinor(), Phone: s.phone, BillRef: s.accountRef,
	})
}

func (s *stk) paidMinor() int64 {
	if s.outcome.PaidMinor > 0 {
		return s.outcome.PaidMinor
	}
	return s.amountMinor
}

// callbackBody is the STK result callback Daraja posts for s.
func (s *stk) callbackBody() []byte {
	cb := map[string]any{
		"MerchantRequestID": s.merchantID,
		"CheckoutRequestID": s.checkoutID,
		"ResultCode":        s.outcome.ResultCode,
		"ResultDesc":        resultDesc(s.outcome.ResultCode),
	}
	if s.outcome.ResultCode == ResultSuccess {
		phone, _ := strconv.ParseInt(s.phone, 10, 64)
		date, _ := strconv.ParseInt(s.completedAt.In(EAT).Format("20060102150405"), 10, 64)
		cb["CallbackMetadata"] = map[string]any{"Item": []map[string]any{
			{"Name": "Amount", "Value": json.Number(formatMinor(s.paidMinor()))},
			{"Name": "MpesaReceiptNumber", "Value": s.receipt},
			{"Name": "Balance"},
			{"Name": "TransactionDate", "Value": date},
			{"Name": "PhoneNumber", "Value": phone},
		}}
	}
	body, _ := json.Marshal(map[string]any{"Body": map[string]any{"stkCallback": cb}})
	return body
}

// sendCallback posts s's result to its callback URL and records it.
func (f *Fake) sendCallback(s *stk) (int, error) {
	status, err := f.post(s.callbackURL, s.callbackBody())
	f.mu.Lock()
	f.callbacks = append(f.callbacks, CallbackResult{CheckoutRequestID: s.checkoutID, Status: status, Err: err})
	f.mu.Unlock()
	return status, err
}

func (f *Fake) stkQuery(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
		return
	}
	var req struct {
		BusinessShortCode string `json:"BusinessShortCode"`
		Password          string `json:"Password"`
		Timestamp         string `json:"Timestamp"`
		CheckoutRequestID string `json:"CheckoutRequestID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - Invalid JSON")
		return
	}
	if !f.checkPassword(req.BusinessShortCode, req.Password, req.Timestamp) {
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - Invalid Password")
		return
	}
	f.mu.Lock()
	s, ok := f.stks[req.CheckoutRequestID]
	var final bool
	if ok {
		final = s.final
	}
	f.mu.Unlock()
	switch {
	case !ok || s.shortCode != req.BusinessShortCode:
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - Invalid CheckoutRequestID")
	case !final:
		writeError(w, http.StatusInternalServerError, ErrCodeProcessing, "The transaction is being processed")
	default:
		writeJSON(w, http.StatusOK, map[string]string{
			"ResponseCode":        "0",
			"ResponseDescription": "The service request has been accepted successsfully",
			"MerchantRequestID":   s.merchantID,
			"CheckoutRequestID":   s.checkoutID,
			"ResultCode":          strconv.Itoa(s.outcome.ResultCode),
			"ResultDesc":          resultDesc(s.outcome.ResultCode),
		})
	}
}

func (f *Fake) registerURL(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
		return
	}
	var req struct {
		ShortCode       string `json:"ShortCode"`
		ResponseType    string `json:"ResponseType"`
		ConfirmationURL string `json:"ConfirmationURL"`
		ValidationURL   string `json:"ValidationURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - Invalid JSON")
		return
	}
	f.mu.Lock()
	_, known := f.passkeys[req.ShortCode]
	f.mu.Unlock()
	switch {
	case !known:
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - Invalid ShortCode")
		return
	case req.ResponseType != "Completed" && req.ResponseType != "Cancelled":
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - Invalid ResponseType")
		return
	case req.ConfirmationURL == "" || req.ValidationURL == "":
		writeError(w, http.StatusBadRequest, ErrCodeBadRequest, "Bad Request - Invalid URL")
		return
	}
	f.mu.Lock()
	f.c2b[req.ShortCode] = c2bURLs{confirmation: req.ConfirmationURL, validation: req.ValidationURL, responseType: req.ResponseType}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{
		"OriginatorCoversationID": randomHex(8),
		"ResponseCode":            "0",
		"ResponseDescription":     "Success",
	})
}

// C2BPayment is a customer paying a paybill from their phone.
type C2BPayment struct {
	ShortCode   string
	BillRef     string
	AmountMinor int64 // whole shillings, in minor units
	Phone       string
	Duplicate   bool // the confirmation is sent twice
}

// C2BResult is what happened to a C2B payment.
type C2BResult struct {
	TransID            string
	Accepted           bool // the validation URL accepted it, or was unreachable and ResponseType is Completed
	ValidationStatus   int
	ConfirmationStatus int // of the last confirmation sent
}

// Pay makes a C2B payment: it posts the validation call, then, unless
// the payment was rejected, the confirmation (twice if Duplicate).
func (f *Fake) Pay(p C2BPayment) (C2BResult, error) {
	f.mu.Lock()
	urls, ok := f.c2b[p.ShortCode]
	f.mu.Unlock()
	if !ok {
		return C2BResult{}, fmt.Errorf("daraja fake: no C2B URLs registered for %s", p.ShortCode)
	}
	if p.AmountMinor <= 0 || p.AmountMinor%100 != 0 {
		return C2BResult{}, fmt.Errorf("daraja fake: C2B amounts are whole shillings")
	}

	f.mu.Lock()
	at := f.now()
	balance := f.balanceLocked(p.ShortCode) + p.AmountMinor
	f.mu.Unlock()
	res := C2BResult{TransID: newReceipt()}
	body := c2bBody(res.TransID, at, p, balance)

	status, accepted, err := f.validate(urls.validation, body)
	res.ValidationStatus = status
	if err != nil || status != http.StatusOK {
		accepted = urls.responseType == "Completed"
	}
	res.Accepted = accepted
	if !accepted {
		return res, nil
	}

	f.mu.Lock()
	f.txns = append(f.txns, Txn{Receipt: res.TransID, ShortCode: p.ShortCode, Kind: "c2b", At: at, AmountMinor: p.AmountMinor, Phone: p.Phone, BillRef: p.BillRef})
	f.mu.Unlock()
	sends := 1
	if p.Duplicate {
		sends = 2
	}
	for range sends {
		res.ConfirmationStatus, err = f.post(urls.confirmation, body)
		if err != nil {
			return res, fmt.Errorf("daraja fake: posting C2B confirmation: %w", err)
		}
	}
	return res, nil
}

func c2bBody(transID string, at time.Time, p C2BPayment, balanceMinor int64) []byte {
	body, _ := json.Marshal(map[string]string{
		"TransactionType":   "Pay Bill",
		"TransID":           transID,
		"TransTime":         at.In(EAT).Format("20060102150405"),
		"TransAmount":       formatMinor(p.AmountMinor),
		"BusinessShortCode": p.ShortCode,
		"BillRefNumber":     p.BillRef,
		"InvoiceNumber":     "",
		"OrgAccountBalance": formatMinor(balanceMinor),
		"ThirdPartyTransID": "",
		"MSISDN":            p.Phone,
		"FirstName":         "Test",
		"MiddleName":        "",
		"LastName":          "Payer",
	})
	return body
}

// validate posts a C2B validation call and reads whether it was accepted.
func (f *Fake) validate(url string, body []byte) (int, bool, error) {
	resp, err := f.client.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec,noctx // G107: the URL is the one the test registered
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		ResultCode json.Number `json:"ResultCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return resp.StatusCode, false, nil
	}
	return resp.StatusCode, out.ResultCode.String() == "0", nil
}

func (f *Fake) post(url string, body []byte) (int, error) {
	resp, err := f.client.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec,noctx // G107: the URL is the one the test registered
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// balanceLocked is shortCode's balance from what the fake moved;
// f.mu must be held.
func (f *Fake) balanceLocked(shortCode string) int64 {
	var sum int64
	for _, t := range f.txns {
		if t.ShortCode == shortCode {
			sum += t.AmountMinor
		}
	}
	return sum
}

// StatementLine is one row of an org-portal statement.
type StatementLine struct {
	Receipt      string
	At           time.Time
	Details      string
	PaidInMinor  int64
	WithdrawnMin int64
	OtherParty   string
	AccountNo    string
}

// StatementOptions changes a statement for reconciliation tests.
type StatementOptions struct {
	Omit  []string        // receipts left out, as if the rail never had them
	Extra []StatementLine // lines VASPRO has no record of
}

// StatementHeader is the assumed org-portal CSV header (OQ-22).
var StatementHeader = []string{
	"Receipt No.", "Completion Time", "Initiation Time", "Details", "Transaction Status",
	"Paid In", "Withdrawn", "Balance", "Balance Confirmed", "Reason Type",
	"Other Party Info", "Linked Transaction ID", "A/C No.",
}

// Statement renders shortCode's statement for the Nairobi day holding
// day, as the org portal's CSV export.
func (f *Fake) Statement(shortCode string, day time.Time, opts StatementOptions) []byte {
	y, m, d := day.In(EAT).Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, EAT)
	end := start.AddDate(0, 0, 1)

	f.mu.Lock()
	var lines []StatementLine
	for _, t := range f.txns {
		if t.ShortCode != shortCode || t.At.Before(start) || !t.At.Before(end) || slices.Contains(opts.Omit, t.Receipt) {
			continue
		}
		details := "Pay Bill from " + t.Phone + " - Test Payer Acc. " + t.BillRef
		if t.Kind == "stk" {
			details = "Pay Bill Online from " + t.Phone + " - Test Payer Acc. " + t.BillRef
		}
		lines = append(lines, StatementLine{
			Receipt: t.Receipt, At: t.At, Details: details, PaidInMinor: t.AmountMinor,
			OtherParty: t.Phone + " - Test Payer", AccountNo: t.BillRef,
		})
	}
	f.mu.Unlock()
	lines = append(lines, opts.Extra...)
	slices.SortStableFunc(lines, func(a, b StatementLine) int { return a.At.Compare(b.At) })

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(StatementHeader)
	var balance int64
	for _, l := range lines {
		balance += l.PaidInMinor - l.WithdrawnMin
		stamp := l.At.In(EAT).Format("02-01-2006 15:04:05")
		_ = w.Write([]string{
			l.Receipt, stamp, stamp, l.Details, "Completed",
			minorOrEmpty(l.PaidInMinor), minorOrEmpty(l.WithdrawnMin), formatMinor(balance), "true", "",
			l.OtherParty, "", l.AccountNo,
		})
	}
	w.Flush()
	return buf.Bytes()
}

func minorOrEmpty(m int64) string {
	if m == 0 {
		return ""
	}
	return formatMinor(m)
}

// formatMinor writes minor units as shillings with two decimals.
func formatMinor(m int64) string {
	sign := ""
	if m < 0 {
		sign, m = "-", -m
	}
	return fmt.Sprintf("%s%d.%02d", sign, m/100, m%100)
}

func resultDesc(code int) string {
	if d, ok := resultDescs[code]; ok {
		return d
	}
	return "The transaction failed."
}

const receiptAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// newReceipt makes an M-Pesa-shaped receipt: 10 upper-case letters and digits.
func newReceipt() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("daraja fake: no randomness"))
	}
	for i := range b {
		b[i] = receiptAlphabet[int(b[i])%len(receiptAlphabet)]
	}
	return string(b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("daraja fake: no randomness"))
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"requestId": randomHex(8), "errorCode": code, "errorMessage": msg})
}
