package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestParseTokens(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"10000", true},
		{"0.5", true},
		{"0.000001", true},
		{"1000000000000", true},
		{"0", false},
		{"-1", false},
		{"0.0000001", false}, // finer than numeric(18,6)
		{"1000000000000.000001", false},
		{"", false},
		{"ten", false},
		{"1e3", false}, // plain decimal notation only
	} {
		t.Run(tc.in, func(t *testing.T) {
			_, err := domain.ParseTokens(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && !errors.Is(err, domain.ErrInvalidTokens) {
				t.Fatalf("got %v, want ErrInvalidTokens", err)
			}
		})
	}
}

func TestMicroTokens_RoundTrip(t *testing.T) {
	for _, s := range []string{"1", "0.000001", "9620", "123.456789", "1000000000000"} {
		d := dec(s)
		if got := domain.TokensFromMicro(domain.MicroTokens(d)); !got.Equal(d) {
			t.Fatalf("%s → %d → %s", s, domain.MicroTokens(d), got)
		}
	}
}

// TestCostMinor: tokens × price, half-up to whole minor units (OQ-15).
func TestCostMinor(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tokens, price string
		want          int64
	}{
		{"worked example: 10,000 at the 0.80 deal", "10000", "80", 800000},
		{"the renegotiated 0.50 deal", "10000", "50", 500000},
		{"a half rounds up", "333", "80.5", 26807},
		{"just under one minor unit rounds to one", "3", "0.333333", 1},
		{"under half a minor unit rounds to nothing", "1", "0.4", 0},
		{"a fractional price and fractional tokens", "2.5", "40.2", 101},
		{"UGX: whole shillings", "7", "12.5", 88},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.CostMinor(dec(tc.tokens), dec(tc.price))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("CostMinor(%s × %s) = %d, want %d", tc.tokens, tc.price, got, tc.want)
			}
		})
	}
	if _, err := domain.CostMinor(dec("1000000000000"), dec("999999999999")); err == nil {
		t.Fatal("a cost beyond int64 was accepted")
	}
}

func TestSelectPrice(t *testing.T) {
	client, other := uuid.New(), uuid.New()
	nov1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	jan1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	mar31 := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)

	row := func(tenant *uuid.UUID, minTokens, price, status string, from time.Time, to *time.Time) domain.PriceRow {
		return domain.PriceRow{ID: uuid.New(), TenantID: tenant, MinTokens: dec(minTokens), MinorPerToken: dec(price), Status: status, From: from, To: to}
	}
	list0 := row(nil, "0", "100", "active", jan1, nil)
	list100k := row(nil, "100000", "90", "active", jan1, nil)
	deal := row(&client, "0", "80", "active", jan1, &nov1)
	newDeal := row(&client, "0", "50", "active", nov1, nil)
	newDealBig := row(&client, "50000", "45", "active", nov1, nil)
	endedDeal := row(&client, "0", "70", "active", jan1, &mar31)
	pendingDeal := row(&client, "0", "10", "pending_approval", jan1, nil)
	withdrawnDeal := row(&client, "0", "20", "withdrawn", jan1, nil)
	futureDeal := row(&client, "0", "30", "active", at.Add(time.Hour), nil)
	othersDeal := row(&other, "0", "40", "active", jan1, nil)
	dealOnlyBig := row(&client, "50000", "60", "active", jan1, nil)

	cases := []struct {
		name string
		rows []domain.PriceRow
		qty  string
		want *domain.PriceRow
	}{
		{"a deal in force wins over the list", []domain.PriceRow{list0, list100k, newDeal}, "10000", &newDeal},
		{"the deal's tier matching the quantity", []domain.PriceRow{list0, newDeal, newDealBig}, "60000", &newDealBig},
		{"below every deal tier, the deal's lowest tier still applies", []domain.PriceRow{list0, dealOnlyBig}, "10", &dealOnlyBig},
		{"without a deal, the list tier matching the quantity", []domain.PriceRow{list0, list100k}, "150000", &list100k},
		{"without a deal, a smaller purchase gets the base list tier", []domain.PriceRow{list0, list100k}, "99999.999999", &list0},
		{"a deal that has ended falls back to the list", []domain.PriceRow{list0, endedDeal}, "10", &list0},
		{"a deal that ended at the purchase moment no longer applies", []domain.PriceRow{list0, deal}, "10", &list0},
		{"an unapproved deal is never used", []domain.PriceRow{list0, pendingDeal}, "10", &list0},
		{"a withdrawn deal is never used", []domain.PriceRow{list0, withdrawnDeal}, "10", &list0},
		{"a deal not yet in force is ignored", []domain.PriceRow{list0, futureDeal}, "10", &list0},
		{"another client's deal is ignored", []domain.PriceRow{list0, othersDeal}, "10", &list0},
		{"no price at all", []domain.PriceRow{pendingDeal}, "10", nil},
		{"a list with no tier low enough", []domain.PriceRow{list100k}, "10", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			when := at
			if tc.name == "a deal that ended at the purchase moment no longer applies" {
				when = nov1
			}
			got, err := domain.SelectPrice(tc.rows, client, when, dec(tc.qty))
			if tc.want == nil {
				if !errors.Is(err, domain.ErrNoPrice) {
					t.Fatalf("got %+v, %v; want ErrNoPrice", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != tc.want.ID {
				t.Fatalf("chose %s at %s, want %s", got.ID, got.MinorPerToken, tc.want.MinorPerToken)
			}
		})
	}
}
