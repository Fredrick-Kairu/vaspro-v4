package domain_test

import (
	"errors"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

var (
	mainKES     = domain.AccountKey{Type: domain.MainWallet, Currency: "KES"}
	smsKES      = domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "sms", Currency: "KES"}
	bonusKES    = domain.AccountKey{Type: domain.Bonus, Currency: "KES"}
	recvKES     = domain.AccountKey{Type: domain.Receivable, Currency: "KES"}
	mpesaKES    = domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_ke", Currency: "KES"}
	revenueKES  = domain.AccountKey{Type: domain.Revenue, ServiceCode: "sms", Currency: "KES"}
	vatKES      = domain.AccountKey{Type: domain.TaxPayable, Currency: "KES"}
	suspenseKES = domain.AccountKey{Type: domain.Suspense, Currency: "KES"}
)

func d(k domain.AccountKey, amount int64) domain.Line {
	return domain.Line{Account: k, Direction: domain.Debit, AmountMinor: amount}
}
func c(k domain.AccountKey, amount int64) domain.Line {
	return domain.Line{Account: k, Direction: domain.Credit, AmountMinor: amount}
}

func withTax(l domain.Line, code string) domain.Line {
	l.TaxCode = code
	return l
}

func withTokens(l domain.Line, tokens string) domain.Line {
	l.Tokens = tokens
	return l
}

func TestValidateAccountKey(t *testing.T) {
	cases := []struct {
		name string
		key  domain.AccountKey
		ok   bool
	}{
		{"a main wallet", mainKES, true},
		{"an SMS service wallet", smsKES, true},
		{"a restricted bonus", domain.AccountKey{Type: domain.Bonus, ServiceCode: "sms", Currency: "KES"}, true},
		{"M-Pesa clearing", mpesaKES, true},
		{"an unknown type", domain.AccountKey{Type: "cash", Currency: "KES"}, false},
		{"a service wallet without a service", domain.AccountKey{Type: domain.ServiceWallet, Currency: "KES"}, false},
		{"revenue without a service", domain.AccountKey{Type: domain.Revenue, Currency: "KES"}, false},
		{"clearing without a rail", domain.AccountKey{Type: domain.ProviderClearing, Currency: "KES"}, false},
		{"a rail on a main wallet", domain.AccountKey{Type: domain.MainWallet, Rail: "mpesa_ke", Currency: "KES"}, false},
		{"a service code with capitals", domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "SMS", Currency: "KES"}, false},
		{"a service code with a space", domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "sms bulk", Currency: "KES"}, false},
		{"a lower-case currency", domain.AccountKey{Type: domain.MainWallet, Currency: "kes"}, false},
		{"no currency", domain.AccountKey{Type: domain.MainWallet}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.ValidateAccountKey(tc.key)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && !errors.Is(err, domain.ErrInvalidPosting) {
				t.Fatalf("got %v, want ErrInvalidPosting", err)
			}
		})
	}
}

func TestValidatePosting(t *testing.T) {
	cases := []struct {
		name string
		p    domain.Posting
		want error
	}{
		{"a top-up", domain.Posting{Type: domain.TxnTopup, Lines: []domain.Line{d(mpesaKES, 1300000), c(mainKES, 1300000)}}, nil},
		{"a token purchase (internal)", domain.Posting{Type: domain.TxnTokenPurchase, Internal: true, Lines: []domain.Line{d(mainKES, 800000), c(smsKES, 800000)}}, nil},
		{"a usage settlement (internal)", domain.Posting{Type: domain.TxnUsage, Internal: true, Lines: []domain.Line{d(smsKES, 30400), c(revenueKES, 26207), c(vatKES, 4193)}}, nil},
		{"a system posting between VASPRO accounts", domain.Posting{Type: domain.TxnAdjustment, System: true, Lines: []domain.Line{d(mpesaKES, 5), c(suspenseKES, 5)}}, nil},
		{"a reversal may move a service wallet back to the main wallet", domain.Posting{Type: domain.TxnReversal, Internal: true, Lines: []domain.Line{d(smsKES, 100), c(mainKES, 100)}}, nil},

		{"one line", domain.Posting{Type: domain.TxnTopup, Lines: []domain.Line{c(mainKES, 1)}}, domain.ErrInvalidPosting},
		{"no lines", domain.Posting{Type: domain.TxnTopup}, domain.ErrInvalidPosting},
		{"an unknown type", domain.Posting{Type: "gift", Lines: []domain.Line{d(mpesaKES, 1), c(mainKES, 1)}}, domain.ErrInvalidPosting},
		{"a zero line", domain.Posting{Type: domain.TxnTopup, Lines: []domain.Line{d(mpesaKES, 0), c(mainKES, 0)}}, domain.ErrInvalidPosting},
		{"a negative line", domain.Posting{Type: domain.TxnTopup, Lines: []domain.Line{d(mpesaKES, -5), c(mainKES, -5)}}, domain.ErrInvalidPosting},
		{"a direction other than D or C", domain.Posting{Type: domain.TxnTopup, Lines: []domain.Line{{Account: mpesaKES, Direction: 'X', AmountMinor: 1}, c(mainKES, 1)}}, domain.ErrInvalidPosting},
		{"a bad account key", domain.Posting{Type: domain.TxnTopup, Lines: []domain.Line{d(domain.AccountKey{Type: domain.ProviderClearing, Currency: "KES"}, 1), c(mainKES, 1)}}, domain.ErrInvalidPosting},
		{"a system posting naming a client account", domain.Posting{Type: domain.TxnAdjustment, System: true, Lines: []domain.Line{d(mpesaKES, 1), c(mainKES, 1)}}, domain.ErrInvalidPosting},

		{"a service wallet line in a generic posting", domain.Posting{Type: domain.TxnTokenPurchase, Lines: []domain.Line{d(mainKES, 800000), c(smsKES, 800000)}}, domain.ErrInvalidPosting},
		{"tokens on a service wallet line", domain.Posting{Type: domain.TxnTokenPurchase, Internal: true, Lines: []domain.Line{d(mainKES, 800000), withTokens(c(smsKES, 800000), "10000")}}, nil},
		{"tokens on a main wallet line", domain.Posting{Type: domain.TxnTopup, Lines: []domain.Line{d(mpesaKES, 1), withTokens(c(mainKES, 1), "1")}}, domain.ErrInvalidPosting},
		{"malformed tokens", domain.Posting{Type: domain.TxnTokenPurchase, Internal: true, Lines: []domain.Line{d(mainKES, 1), withTokens(c(smsKES, 1), "1e3")}}, domain.ErrInvalidPosting},
		{"a tax code on a tax line", domain.Posting{Type: domain.TxnUsage, Lines: []domain.Line{d(mainKES, 100), c(revenueKES, 86), withTax(c(vatKES, 14), "VAT")}}, nil},
		{"a tax code on a revenue line", domain.Posting{Type: domain.TxnUsage, Lines: []domain.Line{d(mainKES, 100), withTax(c(revenueKES, 100), "VAT")}}, domain.ErrInvalidPosting},
		{"a malformed tax code", domain.Posting{Type: domain.TxnUsage, Lines: []domain.Line{d(mainKES, 100), withTax(c(vatKES, 100), "vat 16%")}}, domain.ErrInvalidPosting},
		{"allocation is final: service wallet back to main", domain.Posting{Type: domain.TxnRefund, Internal: true, Lines: []domain.Line{d(smsKES, 100), c(mainKES, 100)}}, domain.ErrAllocationFinal},
		{"allocation is final: bonus to main", domain.Posting{Type: domain.TxnAdjustment, Lines: []domain.Line{d(bonusKES, 100), c(mainKES, 100)}}, domain.ErrAllocationFinal},
		{"allocation is final even through a third account", domain.Posting{Type: domain.TxnAdjustment, Internal: true, Lines: []domain.Line{d(smsKES, 100), c(suspenseKES, 100), d(suspenseKES, 100), c(mainKES, 100)}}, domain.ErrAllocationFinal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.ValidatePosting(tc.p)
			if tc.want == nil && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestValidatePosting_Balanced: a posting must balance per currency
// before it reaches the database, whose deferred check (3.2) is the
// backstop.
func TestValidatePosting_Balanced(t *testing.T) {
	ugxMain := domain.AccountKey{Type: domain.MainWallet, Currency: "UGX"}
	ugxClearing := domain.AccountKey{Type: domain.ProviderClearing, Rail: "mtn_ug", Currency: "UGX"}
	cases := []struct {
		name  string
		lines []domain.Line
		ok    bool
	}{
		{"more debit than credit", []domain.Line{d(mpesaKES, 2), c(mainKES, 1)}, false},
		{"balanced across currencies only", []domain.Line{d(mpesaKES, 5), c(ugxMain, 5)}, false},
		{"each currency balanced", []domain.Line{d(mpesaKES, 5), c(mainKES, 5), d(ugxClearing, 9), c(ugxMain, 9)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.ValidatePosting(domain.Posting{Type: domain.TxnTopup, Lines: tc.lines})
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && !errors.Is(err, domain.ErrInvalidPosting) {
				t.Fatalf("got %v, want ErrInvalidPosting", err)
			}
		})
	}
}

func TestNetDeltas(t *testing.T) {
	lines := []domain.Line{
		d(smsKES, 30400), c(revenueKES, 26207), c(vatKES, 4193),
		c(smsKES, 400), d(recvKES, 50),
	}
	got, err := domain.NetDeltas(lines)
	if err != nil {
		t.Fatal(err)
	}
	want := map[domain.AccountKey]int64{
		smsKES:     -30000, // credit-normal: debit lowers, credit raises
		revenueKES: 26207,
		vatKES:     4193,
		recvKES:    50, // debit-normal
	}
	if len(got) != len(want) {
		t.Fatalf("NetDeltas = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%+v: %d, want %d", k, got[k], v)
		}
	}
}
