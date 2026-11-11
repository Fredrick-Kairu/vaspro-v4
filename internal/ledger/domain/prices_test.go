package domain_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

func TestValidateNewPrice(t *testing.T) {
	client := uuid.New()
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	before, after := from.Add(-time.Hour), from.Add(time.Hour)
	deal := func() domain.NewPrice {
		return domain.NewPrice{TenantID: &client, ServiceCode: "sms", Currency: "KES", MinorPerToken: "50", DealReference: "Q-2026-114", From: from}
	}
	list := func() domain.NewPrice {
		return domain.NewPrice{ServiceCode: "sms", Currency: "KES", MinorPerToken: "80", From: from}
	}
	with := func(p domain.NewPrice, f func(*domain.NewPrice)) domain.NewPrice { f(&p); return p }

	cases := []struct {
		name string
		p    domain.NewPrice
		ok   bool
	}{
		{"a deal", deal(), true},
		{"a list price", list(), true},
		{"a volume tier with fractions", with(list(), func(p *domain.NewPrice) { p.MinTokens = "100000.5"; p.MinorPerToken = "79.123456" }), true},
		{"a deal with an end", with(deal(), func(p *domain.NewPrice) { p.To = &after }), true},
		{"a deal without its reference", with(deal(), func(p *domain.NewPrice) { p.DealReference = "" }), false},
		{"a list price with a deal reference", with(list(), func(p *domain.NewPrice) { p.DealReference = "Q-1" }), false},
		{"a zero price", with(list(), func(p *domain.NewPrice) { p.MinorPerToken = "0" }), false},
		{"a price finer than numeric(18,6)", with(list(), func(p *domain.NewPrice) { p.MinorPerToken = "0.0000001" }), false},
		{"a negative tier", with(list(), func(p *domain.NewPrice) { p.MinTokens = "-1" }), false},
		{"a tier that isn't a decimal", with(list(), func(p *domain.NewPrice) { p.MinTokens = "1e5" }), false},
		{"an end before the start", with(deal(), func(p *domain.NewPrice) { p.To = &before }), false},
		{"an end at the start", with(deal(), func(p *domain.NewPrice) { p.To = &from }), false},
		{"no start", with(deal(), func(p *domain.NewPrice) { p.From = time.Time{} }), false},
		{"a bad service code", with(list(), func(p *domain.NewPrice) { p.ServiceCode = "SMS" }), false},
		{"a bad currency", with(list(), func(p *domain.NewPrice) { p.Currency = "kes" }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.ValidateNewPrice(tc.p)
			if tc.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got.MinTokens.IsNegative() || !got.MinorPerToken.IsPositive() {
					t.Fatalf("parsed %+v", got)
				}
				return
			}
			if !errors.Is(err, domain.ErrInvalidPrice) {
				t.Fatalf("got %v, want ErrInvalidPrice", err)
			}
		})
	}
}

// PricesInForce lists what SelectPrice chooses from (task 3.9): the
// deal's tiers while a deal is in force, else the list's.
func TestPricesInForce(t *testing.T) {
	client, other := uuid.New(), uuid.New()
	jan1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nov1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	row := func(tenant *uuid.UUID, minTokens, price, status string, from time.Time, to *time.Time) domain.PriceRow {
		return domain.PriceRow{ID: uuid.New(), TenantID: tenant, MinTokens: dec(minTokens), MinorPerToken: dec(price), Status: status, From: from, To: to}
	}
	list0, list100k := row(nil, "0", "100", "active", jan1, nil), row(nil, "100000", "90", "active", jan1, nil)
	deal0, deal50k := row(&client, "0", "50", "active", nov1, nil), row(&client, "50000", "45", "active", nov1, nil)
	ended := row(&client, "0", "80", "active", jan1, &nov1)
	pending := row(&client, "0", "10", "pending_approval", jan1, nil)
	withdrawn := row(&client, "0", "20", "withdrawn", jan1, nil)
	others := row(&other, "0", "40", "active", jan1, nil)

	ids := func(rows []domain.PriceRow) []uuid.UUID {
		out := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			out[i] = r.ID
		}
		return out
	}
	tests := []struct {
		name string
		rows []domain.PriceRow
		want []domain.PriceRow
	}{
		{"the list's tiers, lowest first", []domain.PriceRow{list100k, list0}, []domain.PriceRow{list0, list100k}},
		{"a deal in force hides the list", []domain.PriceRow{list0, list100k, deal50k, deal0}, []domain.PriceRow{deal0, deal50k}},
		{"an ended deal brings the list back", []domain.PriceRow{list0, ended}, []domain.PriceRow{list0}},
		{"pending, withdrawn and other clients' rows never count", []domain.PriceRow{list0, pending, withdrawn, others}, []domain.PriceRow{list0}},
		{"no rows", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(domain.PricesInForce(tc.rows, client, at))
			want := ids(tc.want)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

// The list and the quote can't disagree: whatever SelectPrice picks is
// among PricesInForce's rows; with no price for the quantity, every
// listed row is a list tier above it (a deal always applies).
func TestPricesInForce_AgreesWithSelectPrice(t *testing.T) {
	client := uuid.New()
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewPCG(7, 7)) //nolint:gosec // a reproducible test sequence
	statuses := []string{"active", "active", "pending_approval", "withdrawn"}
	for range 2000 {
		var rows []domain.PriceRow
		for range rng.IntN(5) {
			var tenant *uuid.UUID
			if rng.IntN(2) == 0 {
				tenant = &client
			}
			from := at.Add(time.Duration(rng.IntN(48)-36) * time.Hour)
			var to *time.Time
			if rng.IntN(3) == 0 {
				end := from.Add(time.Duration(rng.IntN(48)+1) * time.Hour)
				to = &end
			}
			rows = append(rows, domain.PriceRow{ID: uuid.New(), TenantID: tenant, MinTokens: decimal.NewFromInt(int64(rng.IntN(3) * 1000)),
				MinorPerToken: decimal.NewFromInt(int64(rng.IntN(90) + 10)), Status: statuses[rng.IntN(len(statuses))], From: from, To: to})
		}
		listed := domain.PricesInForce(rows, client, at)
		qty := decimal.NewFromInt(int64(rng.IntN(4000)))
		picked, err := domain.SelectPrice(rows, client, at, qty)
		if errors.Is(err, domain.ErrNoPrice) {
			for _, r := range listed {
				if r.TenantID != nil || !r.MinTokens.GreaterThan(qty) {
					t.Fatalf("no price for %s, yet %+v is listed", qty, r)
				}
			}
			continue
		}
		found := false
		for _, r := range listed {
			found = found || r.ID == picked.ID
		}
		if !found {
			t.Fatalf("SelectPrice picked %+v, not among the listed %+v", picked, listed)
		}
	}
}
