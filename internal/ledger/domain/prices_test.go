package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

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
