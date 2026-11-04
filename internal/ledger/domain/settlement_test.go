package domain_test

import (
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/money"
)

// TestSplitUsage: data model §5 step 4 and its tax-exclusive variant
// (OQ-16): inclusive splits the value drawn; exclusive adds VAT on top,
// from the main wallet up to its available balance, the rest a debt.
func TestSplitUsage(t *testing.T) {
	cases := []struct {
		name          string
		value         int64
		inclusive     bool
		mainAvailable int64
		currency      money.Currency
		want          domain.UsageSplit
	}{
		{"worked example, VAT included: 304.00 → 262.07 + 41.93", 30400, true, 0, money.KES,
			domain.UsageSplit{Revenue: 26207, VAT: 4193}},
		{"VAT excluded, the main wallet covers it: 304.00 + 48.64", 30400, false, 1000000, money.KES,
			domain.UsageSplit{Revenue: 30400, VAT: 4864, VATFromMain: 4864}},
		{"VAT excluded, the main wallet has 10.00: the rest is a debt", 30400, false, 1000, money.KES,
			domain.UsageSplit{Revenue: 30400, VAT: 4864, VATFromMain: 1000, VATToReceivable: 3864}},
		{"VAT excluded, an empty main wallet", 30400, false, 0, money.KES,
			domain.UsageSplit{Revenue: 30400, VAT: 4864, VATToReceivable: 4864}},
		{"one minor unit, VAT included: all revenue", 1, true, 0, money.KES,
			domain.UsageSplit{Revenue: 1, VAT: 0}},
		{"UGX, VAT included: whole shillings", 100, true, 0, money.UGX,
			domain.UsageSplit{Revenue: 86, VAT: 14}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.SplitUsage(tc.value, 1600, tc.inclusive, tc.mainAvailable, tc.currency)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			// The usage posting balances: the wallet's value (plus the
			// exclusive VAT's sources) equals revenue plus VAT.
			debits := tc.value + got.VATFromMain + got.VATToReceivable
			if debits != got.Revenue+got.VAT {
				t.Fatalf("debits %d, credits %d", debits, got.Revenue+got.VAT)
			}
		})
	}
	if _, err := domain.SplitUsage(0, 1600, true, 0, money.KES); err == nil {
		t.Fatal("a zero value was split")
	}
}

func TestWindowFor(t *testing.T) {
	at := func(h, m, s int) time.Time { return time.Date(2026, 11, 4, h, m, s, 0, time.UTC) }
	for _, tc := range []struct {
		now        time.Time
		start, end time.Time
	}{
		{at(9, 7, 30), at(9, 0, 0), at(9, 5, 0)},
		{at(9, 5, 0), at(9, 0, 0), at(9, 5, 0)},
		{at(9, 4, 59), at(8, 55, 0), at(9, 0, 0)},
	} {
		start, end := domain.WindowFor(tc.now, 5*time.Minute)
		if !start.Equal(tc.start) || !end.Equal(tc.end) {
			t.Errorf("WindowFor(%s) = [%s, %s), want [%s, %s)", tc.now.Format(time.TimeOnly), start.Format(time.TimeOnly), end.Format(time.TimeOnly), tc.start.Format(time.TimeOnly), tc.end.Format(time.TimeOnly))
		}
	}
}
