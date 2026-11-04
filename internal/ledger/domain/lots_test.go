package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

func lot(bought, remaining, price string) domain.Lot {
	return domain.Lot{ID: uuid.New(), Bought: dec(bought), Remaining: dec(remaining), Price: dec(price)}
}

// TestDrawFIFO_WorkedExample: lot A (10,000 at 80c) and lot B (10,000
// at 50c, bought after the new deal). 380 delivered, then 9,620 more:
// all from lot A at 80c (7,696.00), and only then lot B.
func TestDrawFIFO_WorkedExample(t *testing.T) {
	a, b := lot("10000", "10000", "80"), lot("10000", "10000", "50")

	draws, value, err := domain.DrawFIFO([]domain.Lot{a, b}, dec("380"))
	if err != nil {
		t.Fatal(err)
	}
	if len(draws) != 1 || draws[0].LotID != a.ID || value != 30400 {
		t.Fatalf("380 tokens: %+v, value %d; want all from lot A, 30400", draws, value)
	}
	a.Remaining = dec("9620")

	draws, value, err = domain.DrawFIFO([]domain.Lot{a, b}, dec("9620"))
	if err != nil {
		t.Fatal(err)
	}
	if len(draws) != 1 || draws[0].LotID != a.ID || value != 769600 {
		t.Fatalf("9,620 tokens: %+v, value %d; want all from lot A, 769600", draws, value)
	}
	a.Remaining = dec("0")

	draws, value, err = domain.DrawFIFO([]domain.Lot{a, b}, dec("100"))
	if err != nil {
		t.Fatal(err)
	}
	if len(draws) != 1 || draws[0].LotID != b.ID || value != 5000 {
		t.Fatalf("then 100 tokens: %+v, value %d; want from lot B at 50c, 5000", draws, value)
	}
}

func TestDrawFIFO_SpansLotsOldestFirst(t *testing.T) {
	a, b, c := lot("10", "4", "80"), lot("10", "10", "50"), lot("10", "10", "30")
	draws, value, err := domain.DrawFIFO([]domain.Lot{a, b, c}, dec("15"))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		lot    uuid.UUID
		tokens string
	}{{a.ID, "4"}, {b.ID, "10"}, {c.ID, "1"}}
	if len(draws) != len(want) {
		t.Fatalf("draws %+v", draws)
	}
	for i, w := range want {
		if draws[i].LotID != w.lot || !draws[i].Tokens.Equal(dec(w.tokens)) {
			t.Fatalf("draw %d = %+v, want %s tokens from lot %d", i, draws[i], w.tokens, i)
		}
	}
	if value != 4*80+10*50+1*30 {
		t.Fatalf("value %d, want %d", value, 4*80+10*50+1*30)
	}
}

func TestDrawFIFO_NotEnoughTokens(t *testing.T) {
	_, _, err := domain.DrawFIFO([]domain.Lot{lot("10", "3", "80"), lot("10", "2", "50")}, dec("5.000001"))
	if !errors.Is(err, domain.ErrInsufficientTokens) {
		t.Fatalf("got %v, want ErrInsufficientTokens", err)
	}
}

// TestDrawFIFO_DrawsSumToTheLotsCost: however a lot is drawn down, its
// draws add up to exactly what it cost and none is negative (OQ-15).
// Plain per-draw rounding fails this: 5 tokens at 0.5 cost 3 (2.5 up),
// but five draws of 1 token round to 1 each, 5 in all.
func TestDrawFIFO_DrawsSumToTheLotsCost(t *testing.T) {
	prices := []string{"0.5", "0.333333", "80.5", "1", "12.345678", "80"}
	boughts := []string{"1", "3", "5", "7.5", "10", "0.000003"}
	steps := []string{"1", "0.5", "0.333333", "2", "0.000001", "3.25"}
	for _, p := range prices {
		for _, b := range boughts {
			for _, step := range steps {
				if dec(b).Div(dec(step)).GreaterThan(dec("10000")) {
					continue // keep each case under 10,000 draws
				}
				t.Run(fmt.Sprintf("%s tokens at %s drawn %s at a time", b, p, step), func(t *testing.T) {
					l := lot(b, b, p)
					cost, err := domain.CostMinor(l.Bought, l.Price)
					if err != nil {
						t.Fatal(err)
					}
					var sum int64
					for l.Remaining.IsPositive() {
						take := decimal.Min(dec(step), l.Remaining)
						draws, value, err := domain.DrawFIFO([]domain.Lot{l}, take)
						if err != nil {
							t.Fatal(err)
						}
						if value < 0 || draws[0].ValueMinor != value {
							t.Fatalf("draw of %s valued %d", take, value)
						}
						sum += value
						l.Remaining = l.Remaining.Sub(take)
					}
					if sum != cost {
						t.Fatalf("draws sum to %d, the lot cost %d", sum, cost)
					}
				})
			}
		}
	}
}

// TestValueAfter: a token hold's display value is the FIFO value of the
// next tokens after those already held.
func TestValueAfter(t *testing.T) {
	lots := []domain.Lot{lot("100", "100", "80"), lot("100", "100", "50")}
	got, err := domain.ValueAfter(lots, dec("100"), dec("100"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 5000 {
		t.Fatalf("the second 100 tokens are worth %d, want 5000 (lot B)", got)
	}
	if _, err := domain.ValueAfter(lots, dec("150"), dec("51")); !errors.Is(err, domain.ErrInsufficientTokens) {
		t.Fatalf("got %v, want ErrInsufficientTokens", err)
	}
}
