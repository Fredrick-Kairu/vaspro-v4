package adapters_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
)

var nairobi = time.FixedZone("EAT", 3*60*60)

func TestParseSTKCallback(t *testing.T) {
	success := `{"Body":{"stkCallback":{"MerchantRequestID":"m-1","CheckoutRequestID":"ws_CO_1","ResultCode":0,"ResultDesc":"The service request is processed successfully.","CallbackMetadata":{"Item":[{"Name":"Amount","Value":1500.00},{"Name":"MpesaReceiptNumber","Value":"NLJ7RT61SV"},{"Name":"Balance"},{"Name":"TransactionDate","Value":20261111100005},{"Name":"PhoneNumber","Value":254708374149}]}}}}`
	got, err := adapters.ParseSTKCallback([]byte(success), nairobi)
	if err != nil {
		t.Fatal(err)
	}
	want := adapters.STKResult{
		MerchantRequestID: "m-1", CheckoutRequestID: "ws_CO_1", ResultCode: 0,
		ResultDesc: "The service request is processed successfully.", Receipt: "NLJ7RT61SV",
		AmountMinor: 150000, PhoneE164: "+254708374149", PaidAt: time.Date(2026, 11, 11, 7, 0, 5, 0, time.UTC),
	}
	if !got.PaidAt.Equal(want.PaidAt) {
		t.Fatalf("paid at %v, want %v", got.PaidAt, want.PaidAt)
	}
	got.PaidAt = want.PaidAt
	if got != want || !got.Succeeded() || got.ErrorCode() != "" {
		t.Fatalf("got %+v", got)
	}

	failed := `{"Body":{"stkCallback":{"MerchantRequestID":"m-2","CheckoutRequestID":"ws_CO_2","ResultCode":1032,"ResultDesc":"Request cancelled by user"}}}`
	got, err = adapters.ParseSTKCallback([]byte(failed), nairobi)
	if err != nil || got.Succeeded() || got.ResultCode != 1032 || got.ErrorCode() != problem.PAY6503 || got.Receipt != "" {
		t.Fatalf("failed: %+v %v", got, err)
	}

	for name, body := range map[string]string{
		"not JSON":                      `{`,
		"no stkCallback":                `{"Body":{}}`,
		"no checkout ID":                `{"Body":{"stkCallback":{"ResultCode":0}}}`,
		"no result code":                `{"Body":{"stkCallback":{"CheckoutRequestID":"ws_CO_3"}}}`,
		"success without a receipt":     strings.Replace(success, `"MpesaReceiptNumber"`, `"Other"`, 1),
		"success without an amount":     strings.Replace(success, `"Amount"`, `"Other"`, 1),
		"an amount below a cent":        strings.Replace(success, `1500.00`, `1500.001`, 1),
		"a negative amount":             strings.Replace(success, `1500.00`, `-1500`, 1),
		"a transaction date not a date": strings.Replace(success, `20261111100005`, `20261399100005`, 1),
	} {
		if _, err := adapters.ParseSTKCallback([]byte(body), nairobi); !errors.Is(err, adapters.ErrBadCallback) {
			t.Errorf("%s: err %v, want ErrBadCallback", name, err)
		}
	}
}

func TestResultCodes(t *testing.T) {
	for code, want := range map[int]problem.Code{
		daraja.ResultSuccess:           "",
		daraja.ResultCancelled:         problem.PAY6503,
		daraja.ResultTimeout:           problem.PAY6504,
		daraja.ResultInsufficientFunds: problem.PAY6505,
		daraja.ResultWrongPIN:          problem.PAY6506,
		1019:                           problem.PAY6507, // anything else the rail reports
	} {
		r := adapters.STKResult{CheckoutRequestID: "ws_CO_1", ResultCode: code}
		if got := r.ErrorCode(); got != want {
			t.Errorf("result %d: %q, want %q", code, got, want)
		}
	}
	if (adapters.STKResult{Pending: true}).ErrorCode() != "" {
		t.Error("a pending result has no error code")
	}
	for err, want := range map[error]problem.Code{
		adapters.ErrRailTimeout:                            problem.PAY6502,
		adapters.ErrRailUnavailable:                        problem.PAY6507,
		fmt.Errorf("wrapped: %w", adapters.ErrRailRefused): problem.PAY6507,
		adapters.ErrInvalidRequest:                         problem.VAL4000,
		errors.New("something else"):                       problem.SYS9000,
	} {
		if got := adapters.ErrorCodeFor(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}

func TestParseC2BCallback(t *testing.T) {
	body := `{"TransactionType":"Pay Bill","TransID":"RKTQDM7W6S","TransTime":"20261111063845","TransAmount":"1500.00","BusinessShortCode":"600100","BillRefNumber":" abc123xy ","InvoiceNumber":"","OrgAccountBalance":"49197.00","ThirdPartyTransID":"","MSISDN":"254708374149","FirstName":"Test","MiddleName":"","LastName":"Payer"}`
	got, err := adapters.ParseC2BCallback([]byte(body), nairobi)
	if err != nil {
		t.Fatal(err)
	}
	if got.Receipt != "RKTQDM7W6S" || got.AmountMinor != 150000 || got.ShortCode != "600100" || got.BillRef != "abc123xy" ||
		got.OrgBalanceMinor == nil || *got.OrgBalanceMinor != 4919700 || got.PhoneE164 != "+254708374149" ||
		got.PayerName != "Test Payer" || got.TransactionType != "Pay Bill" || !got.PaidAt.Equal(time.Date(2026, 11, 11, 3, 38, 45, 0, time.UTC)) {
		t.Fatalf("got %+v", got)
	}

	hashed := strings.Replace(body, `"254708374149"`, `"2e3f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f"`, 1)
	if got, err := adapters.ParseC2BCallback([]byte(hashed), nairobi); err != nil || got.PhoneE164 != "" {
		t.Fatalf("a masked MSISDN is left out, not refused: %+v %v", got, err)
	}
	noBalance := strings.Replace(body, `"49197.00"`, `""`, 1)
	if got, err := adapters.ParseC2BCallback([]byte(noBalance), nairobi); err != nil || got.OrgBalanceMinor != nil {
		t.Fatalf("no balance: %+v %v", got, err)
	}

	for name, b := range map[string]string{
		"not JSON":         `{`,
		"no receipt":       strings.Replace(body, `"RKTQDM7W6S"`, `""`, 1),
		"no shortcode":     strings.Replace(body, `"600100"`, `""`, 1),
		"amount not money": strings.Replace(body, `"1500.00"`, `"15OO"`, 1),
		"zero amount":      strings.Replace(body, `"1500.00"`, `"0"`, 1),
		"bad time":         strings.Replace(body, `"20261111063845"`, `"yesterday"`, 1),
	} {
		if _, err := adapters.ParseC2BCallback([]byte(b), nairobi); !errors.Is(err, adapters.ErrBadCallback) {
			t.Errorf("%s: err %v, want ErrBadCallback", name, err)
		}
	}
}

func TestC2BResponse(t *testing.T) {
	var out map[string]string
	if err := json.Unmarshal(adapters.C2BResponse(true, ""), &out); err != nil || out["ResultCode"] != "0" || out["ResultDesc"] != "Accepted" {
		t.Fatalf("accept: %v %v", out, err)
	}
	if err := json.Unmarshal(adapters.C2BResponse(false, "C2B00012"), &out); err != nil || out["ResultCode"] != "C2B00012" || out["ResultDesc"] != "Rejected" {
		t.Fatalf("reject: %v %v", out, err)
	}
}
