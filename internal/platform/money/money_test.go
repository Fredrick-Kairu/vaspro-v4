package money_test

import (
	"math/rand"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/money"
)

func TestMoney_String(t *testing.T) {
	cases := []struct {
		minor int64
		cur   money.Currency
		want  string
	}{
		{19000, money.KES, "190.00 KES"},
		{1, money.KES, "0.01 KES"},
		{190, money.UGX, "190 UGX"},
		{0, money.TZS, "0.00 TZS"},
	}
	for _, c := range cases {
		got := money.New(c.minor, c.cur).String()
		if got != c.want {
			t.Errorf("New(%d, %s).String() = %q, want %q", c.minor, c.cur.Code, got, c.want)
		}
	}
}

func TestMoney_AddSub(t *testing.T) {
	a := money.New(100, money.KES)
	b := money.New(50, money.KES)

	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum.Minor() != 150 {
		t.Fatalf("Add = %d, want 150", sum.Minor())
	}

	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff.Minor() != 50 {
		t.Fatalf("Sub = %d, want 50", diff.Minor())
	}
}

func TestMoney_AddSub_CurrencyMismatchErrors(t *testing.T) {
	a := money.New(100, money.KES)
	b := money.New(100, money.UGX)

	if _, err := a.Add(b); err == nil {
		t.Fatal("expected Add across currencies to error")
	}
	if _, err := a.Sub(b); err == nil {
		t.Fatal("expected Sub across currencies to error")
	}
}

func TestAllocate_SumsExactly_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		total   int64
		weights []int64
		want    []int64
	}{
		{"even split", 100, []int64{1, 1, 1}, []int64{34, 33, 33}},
		{"uneven weights", 1000, []int64{1, 2, 3}, []int64{167, 333, 500}},
		{"single weight", 19000, []int64{1}, []int64{19000}},
		{"zero total", 0, []int64{1, 1}, []int64{0, 0}},
		{"more parts than minor units", 3, []int64{1, 1, 1, 1, 1}, []int64{1, 1, 1, 0, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parts, err := money.Allocate(money.New(c.total, money.KES), c.weights)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(parts) != len(c.want) {
				t.Fatalf("got %d parts, want %d", len(parts), len(c.want))
			}
			var sum int64
			for i, p := range parts {
				if p.Minor() != c.want[i] {
					t.Errorf("part[%d] = %d, want %d", i, p.Minor(), c.want[i])
				}
				sum += p.Minor()
			}
			if sum != c.total {
				t.Fatalf("parts sum to %d, want %d", sum, c.total)
			}
		})
	}
}

func TestAllocate_PropertyRandomTotalsAndWeightsSumExactly(t *testing.T) {
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // deterministic seed for a reproducible property test, not a security use

	for range 1000 {
		total := rng.Int63n(1_000_000)
		n := rng.Intn(10) + 1
		weights := make([]int64, n)
		for j := range weights {
			weights[j] = rng.Int63n(1000) + 1
		}

		parts, err := money.Allocate(money.New(total, money.KES), weights)
		if err != nil {
			t.Fatalf("unexpected error (total=%d weights=%v): %v", total, weights, err)
		}

		var sum int64
		for _, p := range parts {
			if p.Minor() < 0 {
				t.Fatalf("negative part %d (total=%d weights=%v)", p.Minor(), total, weights)
			}
			sum += p.Minor()
		}
		if sum != total {
			t.Fatalf("parts summed to %d, want %d (weights=%v)", sum, total, weights)
		}
	}
}

func TestAllocate_RejectsNegativeWeightsOrTotal(t *testing.T) {
	if _, err := money.Allocate(money.New(-1, money.KES), []int64{1}); err == nil {
		t.Fatal("expected a negative total to error")
	}
	if _, err := money.Allocate(money.New(100, money.KES), []int64{1, -1}); err == nil {
		t.Fatal("expected a negative weight to error")
	}
	if _, err := money.Allocate(money.New(100, money.KES), []int64{0, 0}); err == nil {
		t.Fatal("expected all-zero weights to error")
	}
	if _, err := money.Allocate(money.New(100, money.KES), nil); err == nil {
		t.Fatal("expected no weights to error")
	}
}

func TestSplitVATInclusive_WorkedExample(t *testing.T) {
	total := money.New(19000, money.KES) // 190.00

	revenue, vat, err := money.SplitVATInclusive(total, 1600) // 16%
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if revenue.Minor() != 16379 {
		t.Fatalf("revenue = %d, want 16379 (163.79)", revenue.Minor())
	}
	if vat.Minor() != 2621 {
		t.Fatalf("vat = %d, want 2621 (26.21)", vat.Minor())
	}
	sum, err := revenue.Add(vat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum.Minor() != total.Minor() {
		t.Fatalf("revenue + vat = %d, want %d", sum.Minor(), total.Minor())
	}
}

func TestAddVATExclusive_WorkedExample(t *testing.T) {
	base := money.New(19000, money.KES) // 190.00

	total, vat, err := money.AddVATExclusive(base, 1600) // 16%
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total.Minor() != 22040 {
		t.Fatalf("total = %d, want 22040 (220.40)", total.Minor())
	}
	if vat.Minor() != 3040 {
		t.Fatalf("vat = %d, want 3040 (30.40)", vat.Minor())
	}
}

func TestSplitVATInclusive_UGXZeroDecimals(t *testing.T) {
	total := money.New(190, money.UGX) // 190 UGX, no fractional minor units ever

	revenue, vat, err := money.SplitVATInclusive(total, 1600)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sum, err := revenue.Add(vat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum.Minor() != total.Minor() {
		t.Fatalf("revenue + vat = %d, want %d", sum.Minor(), total.Minor())
	}
	if revenue.Currency().Exponent != 0 || vat.Currency().Exponent != 0 {
		t.Fatal("expected UGX results to keep a 0-decimal currency")
	}
}

func TestSplitVATInclusive_RejectsNegativeTotal(t *testing.T) {
	if _, _, err := money.SplitVATInclusive(money.New(-1, money.KES), 1600); err == nil {
		t.Fatal("expected a negative total to error")
	}
}

func TestSplitVATInclusive_RejectsNegativeRate(t *testing.T) {
	if _, _, err := money.SplitVATInclusive(money.New(100, money.KES), -1); err == nil {
		t.Fatal("expected a negative rate to error")
	}
}
