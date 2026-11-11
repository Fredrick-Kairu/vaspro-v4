package domain_test

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
)

// Kenya's rules as the KE pack gives them: KES has 2 decimals; numbers
// are +254 and 9 digits.
var ke = domain.TopupRules{CurrencyExponent: 2, CountryCode: "254", NationalNumberLength: 9}

func TestValidateTopup(t *testing.T) {
	for name, tc := range map[string]struct {
		amount int64
		phone  string
		ok     bool
	}{
		"one shilling":      {100, "+254708374149", true},
		"a round amount":    {1_500_000, "+254708374149", true},
		"cents are refused": {150, "+254708374149", false},
		"zero":              {0, "+254708374149", false},
		"negative":          {-100, "+254708374149", false},
		"no plus":           {100, "254708374149", false},
		"local format":      {100, "0708374149", false},
		"another country":   {100, "+255708374149", false},
		"too short":         {100, "+25470837414", false},
		"too long":          {100, "+2547083741490", false},
		"letters":           {100, "+25470837414X", false},
		"spaces":            {100, "+254 708374149", false},
	} {
		err := domain.ValidateTopup(tc.amount, tc.phone, ke)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !tc.ok && !errors.Is(err, domain.ErrInvalidTopup) {
			t.Errorf("%s: got %v, want ErrInvalidTopup", name, err)
		}
	}
	ugx := domain.TopupRules{CurrencyExponent: 0, CountryCode: "256", NationalNumberLength: 9}
	if err := domain.ValidateTopup(5, "+256712345678", ugx); err != nil {
		t.Errorf("a currency without decimals takes any whole amount: %v", err)
	}
}

func TestCallbackToken(t *testing.T) {
	secret := bytes.Repeat([]byte{1}, 32)
	acct := uuid.New()
	tok := domain.CallbackToken(secret, acct)
	if len(tok) != 64 || !domain.ValidCallbackToken(secret, acct, tok) {
		t.Fatalf("token %q doesn't verify", tok)
	}
	for name, ok := range map[string]bool{
		"another account": domain.ValidCallbackToken(secret, uuid.New(), tok),
		"another secret":  domain.ValidCallbackToken(bytes.Repeat([]byte{2}, 32), acct, tok),
		"truncated":       domain.ValidCallbackToken(secret, acct, tok[:32]),
		"not hex":         domain.ValidCallbackToken(secret, acct, "zz"+tok[2:]),
		"empty":           domain.ValidCallbackToken(secret, acct, ""),
	} {
		if ok {
			t.Errorf("%s verified", name)
		}
	}
}

func TestSourceAllowed(t *testing.T) {
	cidrs, err := domain.ParseCIDRs(" 196.201.214.0/24, 10.0.0.7/32 ,")
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{
		"196.201.214.200": true,
		"10.0.0.7":        true,
		"10.0.0.8":        false,
		"8.8.8.8":         false,
		"::1":             false,
	} {
		if got := domain.SourceAllowed(netip.MustParseAddr(ip), cidrs); got != want {
			t.Errorf("%s: %v, want %v", ip, got, want)
		}
	}
	if domain.SourceAllowed(netip.Addr{}, cidrs) {
		t.Error("no address is never allowed")
	}
	none, err := domain.ParseCIDRs("")
	if err != nil || domain.SourceAllowed(netip.MustParseAddr("196.201.214.200"), none) {
		t.Error("an empty list allows nothing")
	}
	if _, err := domain.ParseCIDRs("196.201.214.0/33"); err == nil {
		t.Error("a bad CIDR is refused")
	}
}

// TestApplySTKResult: every intent status × callback outcome.
func TestApplySTKResult(t *testing.T) {
	success := domain.STKOutcome{ResultCode: 0, Receipt: "NLJ7RT61SV", AmountMinor: 150000}
	short := domain.STKOutcome{ResultCode: 0, Receipt: "NLJ7RT61SV", AmountMinor: 100000}
	cancelled := domain.STKOutcome{ResultCode: 1032, ErrorCode: "PAY-6503"}
	noFunds := domain.STKOutcome{ResultCode: 1, ErrorCode: "PAY-6505"}

	type want struct {
		apply            bool
		status           domain.IntentStatus
		late, mismatched bool
	}
	cases := []struct {
		name   string
		status domain.IntentStatus
		cur    string // current receipt
		out    domain.STKOutcome
		want   want
	}{
		{"success on created", domain.IntentCreated, "", success, want{true, domain.IntentSucceeded, false, false}},
		{"success on pending", domain.IntentPending, "", success, want{true, domain.IntentSucceeded, false, false}},
		{"a short payment on pending", domain.IntentPending, "", short, want{true, domain.IntentSucceeded, false, true}},
		{"success after failed is late", domain.IntentFailed, "", success, want{true, domain.IntentSucceeded, true, false}},
		{"success after expired is late", domain.IntentExpired, "", success, want{true, domain.IntentSucceeded, true, false}},
		{"success after cancelled is late", domain.IntentCancelled, "", success, want{true, domain.IntentSucceeded, true, false}},
		{"a late short payment", domain.IntentExpired, "", short, want{true, domain.IntentSucceeded, true, true}},
		{"the same success again", domain.IntentSucceeded, "NLJ7RT61SV", success, want{false, domain.IntentSucceeded, false, false}},
		{"cancelled on pending", domain.IntentPending, "", cancelled, want{true, domain.IntentCancelled, false, false}},
		{"no funds on pending", domain.IntentPending, "", noFunds, want{true, domain.IntentFailed, false, false}},
		{"no funds on created", domain.IntentCreated, "", noFunds, want{true, domain.IntentFailed, false, false}},
		{"a failure after success changes nothing", domain.IntentSucceeded, "NLJ7RT61SV", noFunds, want{false, domain.IntentSucceeded, false, false}},
		{"a failure after failure changes nothing", domain.IntentFailed, "", cancelled, want{false, domain.IntentFailed, false, false}},
		{"a failure after expiry changes nothing", domain.IntentExpired, "", noFunds, want{false, domain.IntentExpired, false, false}},
	}
	for _, tc := range cases {
		got := domain.ApplySTKResult(domain.IntentState{Status: tc.status, AmountMinor: 150000, Receipt: tc.cur}, tc.out)
		if got.Apply != tc.want.apply || got.Status != tc.want.status || got.Late != tc.want.late || got.AmountMismatch != tc.want.mismatched {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// A second, different receipt on a succeeded intent is two
	// payments: never applied over the first, but reported.
	got := domain.ApplySTKResult(domain.IntentState{Status: domain.IntentSucceeded, AmountMinor: 150000, Receipt: "FIRST00001"}, success)
	if got.Apply || !got.Duplicate {
		t.Errorf("a second receipt: %+v, want not applied and Duplicate", got)
	}
	// Every final status is final; created and pending aren't.
	for _, s := range []domain.IntentStatus{domain.IntentSucceeded, domain.IntentFailed, domain.IntentExpired, domain.IntentCancelled} {
		if !s.Final() {
			t.Errorf("%s isn't final", s)
		}
	}
	if domain.IntentCreated.Final() || domain.IntentPending.Final() {
		t.Error("created or pending is final")
	}
}
