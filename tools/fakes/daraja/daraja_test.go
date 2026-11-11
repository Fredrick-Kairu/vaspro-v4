package daraja_test

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
)

var app = daraja.App{ConsumerKey: "fake-key", ConsumerSecret: "fake-secret", ShortCode: "600100", Passkey: "fake-passkey"}

// receiver records what the fake posts to it.
type receiver struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	answer string // the JSON body it replies with
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{answer: `{"ResultCode":0,"ResultDesc":"Accepted"}`}
	rc.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.bodies = append(rc.bodies, b)
		answer := rc.answer
		rc.mu.Unlock()
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(rc.server.Close)
	return rc
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.bodies)
}

func (rc *receiver) body(i int) map[string]any {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(rc.bodies[i], &m)
	return m
}

func token(t *testing.T, f *daraja.Fake) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.URL()+"/oauth/v1/generate?grant_type=client_credentials", nil)
	req.SetBasicAuth(app.ConsumerKey, app.ConsumerSecret)
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if status := do(t, req, &out); status != http.StatusOK || out.AccessToken == "" || out.ExpiresIn != "3599" {
		t.Fatalf("token: %d %+v", status, out)
	}
	return out.AccessToken
}

func do(t *testing.T, req *http.Request, out any) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the URL is the fake's own httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func timestamp() string { return time.Now().In(daraja.EAT).Format("20060102150405") }

func password(ts string) string {
	return base64.StdEncoding.EncodeToString([]byte(app.ShortCode + app.Passkey + ts))
}

func call(t *testing.T, f *daraja.Fake, tok, path string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.URL()+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	return do(t, req, out)
}

type pushResponse struct {
	CheckoutRequestID string `json:"CheckoutRequestID"`
	MerchantRequestID string `json:"MerchantRequestID"`
	ResponseCode      string `json:"ResponseCode"`
	ErrorCode         string `json:"errorCode"`
}

func push(t *testing.T, f *daraja.Fake, tok, callbackURL string, amount any) (int, pushResponse) {
	t.Helper()
	ts := timestamp()
	var out pushResponse
	status := call(t, f, tok, "/mpesa/stkpush/v1/processrequest", map[string]any{
		"BusinessShortCode": app.ShortCode, "Password": password(ts), "Timestamp": ts,
		"TransactionType": "CustomerPayBillOnline", "Amount": amount,
		"PartyA": daraja.SandboxMSISDN, "PartyB": app.ShortCode, "PhoneNumber": daraja.SandboxMSISDN,
		"CallBackURL": callbackURL, "AccountReference": "ABC123XY", "TransactionDesc": "Top-up",
	}, &out)
	return status, out
}

type queryResponse struct {
	ResultCode string `json:"ResultCode"`
	ErrorCode  string `json:"errorCode"`
}

func query(t *testing.T, f *daraja.Fake, tok, checkoutID string) (int, queryResponse) {
	t.Helper()
	ts := timestamp()
	var out queryResponse
	status := call(t, f, tok, "/mpesa/stkpushquery/v1/query", map[string]any{
		"BusinessShortCode": app.ShortCode, "Password": password(ts), "Timestamp": ts, "CheckoutRequestID": checkoutID,
	}, &out)
	return status, out
}

func stkCallback(m map[string]any) map[string]any {
	return m["Body"].(map[string]any)["stkCallback"].(map[string]any)
}

func TestOAuth(t *testing.T) {
	f := daraja.New(app)
	t.Cleanup(f.Close)

	token(t, f)
	token(t, f)
	if f.TokenIssues() != 2 {
		t.Fatalf("issues %d, want 2", f.TokenIssues())
	}
	req, _ := http.NewRequest(http.MethodGet, f.URL()+"/oauth/v1/generate?grant_type=client_credentials", nil)
	req.SetBasicAuth(app.ConsumerKey, "wrong")
	if status := do(t, req, nil); status != http.StatusBadRequest {
		t.Fatalf("wrong secret: %d", status)
	}

	tok := token(t, f)
	f.ExpireTokens()
	var out pushResponse
	if status := call(t, f, tok, "/mpesa/stkpush/v1/processrequest", map[string]any{}, &out); status != http.StatusUnauthorized || out.ErrorCode != daraja.ErrCodeInvalidToken {
		t.Fatalf("expired token: %d %+v", status, out)
	}
}

func TestSTKPush_Refusals(t *testing.T) {
	f := daraja.New(app)
	t.Cleanup(f.Close)
	tok := token(t, f)

	if status, _ := push(t, f, tok, "http://cb.invalid", "10.5"); status != http.StatusBadRequest {
		t.Fatalf("fractional amount: %d", status)
	}
	if status, _ := push(t, f, tok, "http://cb.invalid", 0); status != http.StatusBadRequest {
		t.Fatalf("zero amount: %d", status)
	}
	ts := timestamp()
	var out pushResponse
	status := call(t, f, tok, "/mpesa/stkpush/v1/processrequest", map[string]any{
		"BusinessShortCode": app.ShortCode, "Password": "wrong", "Timestamp": ts,
		"TransactionType": "CustomerPayBillOnline", "Amount": 10,
		"PartyA": daraja.SandboxMSISDN, "PartyB": app.ShortCode, "PhoneNumber": daraja.SandboxMSISDN,
		"CallBackURL": "http://cb.invalid", "AccountReference": "ABC123XY", "TransactionDesc": "Top-up",
	}, &out)
	if status != http.StatusBadRequest || out.ErrorCode != daraja.ErrCodeBadRequest {
		t.Fatalf("bad password: %d %+v", status, out)
	}
}

func TestSTKPush_Outcomes(t *testing.T) {
	for name, code := range map[string]int{
		"success":            daraja.ResultSuccess,
		"cancelled by user":  daraja.ResultCancelled,
		"timeout":            daraja.ResultTimeout,
		"insufficient funds": daraja.ResultInsufficientFunds,
		"wrong PIN":          daraja.ResultWrongPIN,
	} {
		t.Run(name, func(t *testing.T) {
			f := daraja.New(app)
			t.Cleanup(f.Close)
			rc := newReceiver(t)
			tok := token(t, f)
			f.Script(daraja.SandboxMSISDN, daraja.Outcome{ResultCode: code})

			status, resp := push(t, f, tok, rc.server.URL, "100")
			if status != http.StatusOK || resp.ResponseCode != "0" || resp.CheckoutRequestID == "" {
				t.Fatalf("push: %d %+v", status, resp)
			}
			f.Wait()
			if rc.count() != 1 {
				t.Fatalf("callbacks %d, want 1", rc.count())
			}
			cb := stkCallback(rc.body(0))
			if cb["CheckoutRequestID"] != resp.CheckoutRequestID || cb["ResultCode"] != float64(code) {
				t.Fatalf("callback %+v", cb)
			}
			_, hasMeta := cb["CallbackMetadata"]
			if hasMeta != (code == daraja.ResultSuccess) {
				t.Fatalf("metadata present %v for result %d", hasMeta, code)
			}
			if _, q := query(t, f, tok, resp.CheckoutRequestID); q.ResultCode != strings.TrimSpace(jsonInt(code)) {
				t.Fatalf("query result %q, want %d", q.ResultCode, code)
			}
			txns := f.Transactions()
			if (len(txns) == 1) != (code == daraja.ResultSuccess) {
				t.Fatalf("transactions %+v for result %d", txns, code)
			}
		})
	}
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestSTKPush_SuccessMetadata(t *testing.T) {
	f := daraja.New(app)
	t.Cleanup(f.Close)
	rc := newReceiver(t)
	tok := token(t, f)
	f.Next(daraja.Outcome{ResultCode: daraja.ResultSuccess, PaidMinor: 9000})

	_, resp := push(t, f, tok, rc.server.URL, 100)
	f.Wait()
	items := stkCallback(rc.body(0))["CallbackMetadata"].(map[string]any)["Item"].([]any)
	got := map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		got[m["Name"].(string)] = m["Value"]
	}
	if got["Amount"] != float64(90) || got["PhoneNumber"] != float64(254708374149) || len(got["MpesaReceiptNumber"].(string)) != 10 {
		t.Fatalf("metadata %+v", got)
	}
	txns := f.Transactions()
	if len(txns) != 1 || txns[0].AmountMinor != 9000 || txns[0].Receipt != got["MpesaReceiptNumber"] || resp.CheckoutRequestID == "" {
		t.Fatalf("the paid amount, not the requested one, is recorded: %+v", txns)
	}
}

func TestSTKPush_Deliveries(t *testing.T) {
	t.Run("twice", func(t *testing.T) {
		f := daraja.New(app)
		t.Cleanup(f.Close)
		rc := newReceiver(t)
		tok := token(t, f)
		f.Next(daraja.Outcome{Delivery: daraja.DeliverTwice})
		push(t, f, tok, rc.server.URL, 10)
		f.Wait()
		if rc.count() != 2 || !bytes.Equal(rawBody(rc, 0), rawBody(rc, 1)) {
			t.Fatalf("want two identical callbacks, got %d", rc.count())
		}
		if len(f.Transactions()) != 1 {
			t.Fatal("a duplicate callback isn't a second payment")
		}
	})
	t.Run("before the response", func(t *testing.T) {
		f := daraja.New(app)
		t.Cleanup(f.Close)
		rc := newReceiver(t)
		tok := token(t, f)
		f.Next(daraja.Outcome{Delivery: daraja.DeliverBeforeResponse})
		_, resp := push(t, f, tok, rc.server.URL, 10)
		if rc.count() != 1 || stkCallback(rc.body(0))["CheckoutRequestID"] != resp.CheckoutRequestID {
			t.Fatalf("the callback must have arrived before the push returned: %d", rc.count())
		}
	})
	t.Run("never: only the query knows", func(t *testing.T) {
		f := daraja.New(app)
		t.Cleanup(f.Close)
		rc := newReceiver(t)
		tok := token(t, f)
		f.Next(daraja.Outcome{ResultCode: daraja.ResultSuccess, Delivery: daraja.NoCallback})
		_, resp := push(t, f, tok, rc.server.URL, 10)
		f.Wait()
		if _, q := query(t, f, tok, resp.CheckoutRequestID); rc.count() != 0 || q.ResultCode != "0" {
			t.Fatalf("callbacks %d, query %+v", rc.count(), q)
		}
	})
	t.Run("manual: pending until released", func(t *testing.T) {
		f := daraja.New(app)
		t.Cleanup(f.Close)
		rc := newReceiver(t)
		tok := token(t, f)
		f.Next(daraja.Outcome{ResultCode: daraja.ResultCancelled, Delivery: daraja.Manual})
		_, resp := push(t, f, tok, rc.server.URL, 10)
		if status, q := query(t, f, tok, resp.CheckoutRequestID); status != http.StatusInternalServerError || q.ErrorCode != daraja.ErrCodeProcessing {
			t.Fatalf("pending query: %d %+v", status, q)
		}
		if status, err := f.Release(resp.CheckoutRequestID); err != nil || status != http.StatusOK {
			t.Fatalf("release: %d %v", status, err)
		}
		if _, q := query(t, f, tok, resp.CheckoutRequestID); q.ResultCode != "1032" || rc.count() != 1 {
			t.Fatalf("after release: %+v, %d callbacks", q, rc.count())
		}
		if _, err := f.Release(resp.CheckoutRequestID); err == nil {
			t.Fatal("a second release must fail")
		}
	})
	t.Run("an unknown checkout is refused", func(t *testing.T) {
		f := daraja.New(app)
		t.Cleanup(f.Close)
		tok := token(t, f)
		if status, q := query(t, f, tok, "ws_CO_unknown"); status != http.StatusBadRequest || q.ErrorCode != daraja.ErrCodeBadRequest {
			t.Fatalf("unknown: %d %+v", status, q)
		}
	})
}

func rawBody(rc *receiver, i int) []byte {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.bodies[i]
}

func registerC2B(t *testing.T, f *daraja.Fake, tok, validation, confirmation, responseType string) {
	t.Helper()
	var out struct {
		ResponseCode string `json:"ResponseCode"`
	}
	if status := call(t, f, tok, "/mpesa/c2b/v1/registerurl", map[string]string{
		"ShortCode": app.ShortCode, "ResponseType": responseType, "ConfirmationURL": confirmation, "ValidationURL": validation,
	}, &out); status != http.StatusOK || out.ResponseCode != "0" {
		t.Fatalf("register: %d %+v", status, out)
	}
}

func TestC2B(t *testing.T) {
	t.Run("validation then confirmation", func(t *testing.T) {
		f := daraja.New(app)
		t.Cleanup(f.Close)
		val, conf := newReceiver(t), newReceiver(t)
		registerC2B(t, f, token(t, f), val.server.URL, conf.server.URL, "Completed")

		res, err := f.Pay(daraja.C2BPayment{ShortCode: app.ShortCode, BillRef: "ABC123XY", AmountMinor: 150000, Phone: daraja.SandboxMSISDN, Duplicate: true})
		if err != nil || !res.Accepted || len(res.TransID) != 10 {
			t.Fatalf("pay: %+v %v", res, err)
		}
		if val.count() != 1 || conf.count() != 2 {
			t.Fatalf("validation %d, confirmations %d", val.count(), conf.count())
		}
		c := conf.body(0)
		if c["TransID"] != res.TransID || c["TransAmount"] != "1500.00" || c["BillRefNumber"] != "ABC123XY" || c["OrgAccountBalance"] != "1500.00" {
			t.Fatalf("confirmation %+v", c)
		}
	})
	t.Run("rejected at validation", func(t *testing.T) {
		f := daraja.New(app)
		t.Cleanup(f.Close)
		val, conf := newReceiver(t), newReceiver(t)
		val.answer = `{"ResultCode":"C2B00012","ResultDesc":"Rejected"}`
		registerC2B(t, f, token(t, f), val.server.URL, conf.server.URL, "Completed")
		res, err := f.Pay(daraja.C2BPayment{ShortCode: app.ShortCode, BillRef: "NOPE", AmountMinor: 1000, Phone: daraja.SandboxMSISDN})
		if err != nil || res.Accepted || conf.count() != 0 || len(f.Transactions()) != 0 {
			t.Fatalf("rejected: %+v %v, %d confirmations", res, err, conf.count())
		}
	})
	t.Run("validation unreachable follows ResponseType", func(t *testing.T) {
		for responseType, want := range map[string]bool{"Completed": true, "Cancelled": false} {
			f := daraja.New(app)
			conf := newReceiver(t)
			registerC2B(t, f, token(t, f), "http://127.0.0.1:1/unreachable", conf.server.URL, responseType)
			res, err := f.Pay(daraja.C2BPayment{ShortCode: app.ShortCode, BillRef: "ABC123XY", AmountMinor: 1000, Phone: daraja.SandboxMSISDN})
			if err != nil || res.Accepted != want || (conf.count() == 1) != want {
				t.Fatalf("%s: %+v %v", responseType, res, err)
			}
			f.Close()
		}
	})
}

func TestStatement(t *testing.T) {
	f := daraja.New(app)
	t.Cleanup(f.Close)
	day := time.Date(2026, 11, 11, 10, 0, 0, 0, daraja.EAT)
	now := day
	f.SetNow(func() time.Time { return now })
	rc := newReceiver(t)
	tok := token(t, f)
	registerC2B(t, f, tok, rc.server.URL, rc.server.URL, "Completed")

	f.Next(daraja.Outcome{}, daraja.Outcome{ResultCode: daraja.ResultCancelled})
	push(t, f, tok, rc.server.URL, 100)
	push(t, f, tok, rc.server.URL, 200)
	f.Wait()
	paid, _ := f.Pay(daraja.C2BPayment{ShortCode: app.ShortCode, BillRef: "ABC123XY", AmountMinor: 5000, Phone: daraja.SandboxMSISDN})
	now = day.AddDate(0, 0, 1) // next day: not on this statement
	_, _ = f.Pay(daraja.C2BPayment{ShortCode: app.ShortCode, BillRef: "ABC123XY", AmountMinor: 7000, Phone: daraja.SandboxMSISDN})

	rows := readCSV(t, f.Statement(app.ShortCode, day, daraja.StatementOptions{}))
	if len(rows) != 3 || rows[1][5] != "100.00" || rows[2][0] != paid.TransID || rows[2][5] != "50.00" || rows[2][7] != "150.00" || rows[2][12] != "ABC123XY" {
		t.Fatalf("statement %v", rows)
	}

	stkReceipt := rows[1][0]
	rows = readCSV(t, f.Statement(app.ShortCode, day, daraja.StatementOptions{
		Omit:  []string{stkReceipt},
		Extra: []daraja.StatementLine{{Receipt: "EXTRA00001", At: day.Add(time.Hour), PaidInMinor: 1000}},
	}))
	if len(rows) != 3 || rows[1][0] != paid.TransID || rows[2][0] != "EXTRA00001" || rows[2][7] != "60.00" {
		t.Fatalf("statement with omit and extra %v", rows)
	}
}

func readCSV(t *testing.T, b []byte) [][]string {
	t.Helper()
	rows, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
