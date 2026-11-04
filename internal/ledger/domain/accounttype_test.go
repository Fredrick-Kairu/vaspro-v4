package domain_test

import (
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// TestAccountTypes_NormalSideAndOwnership writes out every account type
// (data model §5) with its normal side, owner and wallet status, as
// agreed on 4 Nov (OQ-13). It fails if AccountTypes grows without this
// table being updated.
func TestAccountTypes_NormalSideAndOwnership(t *testing.T) {
	const (
		C = domain.Credit
		D = domain.Debit
	)
	want := []struct {
		typ          domain.AccountType
		side         domain.Side
		clientOwned  bool
		clientWallet bool
	}{
		{domain.MainWallet, C, true, true},
		{domain.ServiceWallet, C, true, true},
		{domain.Bonus, C, true, true},
		{domain.Receivable, D, true, false},
		{domain.RevenueSharePayable, C, true, false},
		{domain.CommissionPayable, C, true, false},
		{domain.ProviderClearing, D, false, false},
		{domain.Suspense, C, false, false},
		{domain.ReversalClearing, C, false, false},
		{domain.Revenue, C, false, false},
		{domain.TaxPayable, C, false, false},
	}

	if len(domain.AccountTypes) != len(want) {
		t.Fatalf("AccountTypes has %d types, this table %d: update both together", len(domain.AccountTypes), len(want))
	}
	for i, w := range want {
		if domain.AccountTypes[i] != w.typ {
			t.Errorf("AccountTypes[%d] = %q, want %q (enum order)", i, domain.AccountTypes[i], w.typ)
		}
		side, err := domain.NormalSide(w.typ)
		if err != nil {
			t.Errorf("NormalSide(%q): %v", w.typ, err)
		}
		if side != w.side {
			t.Errorf("NormalSide(%q) = %q, want %q", w.typ, side, w.side)
		}
		if got := domain.IsClientOwned(w.typ); got != w.clientOwned {
			t.Errorf("IsClientOwned(%q) = %v, want %v", w.typ, got, w.clientOwned)
		}
		if got := domain.IsClientWallet(w.typ); got != w.clientWallet {
			t.Errorf("IsClientWallet(%q) = %v, want %v", w.typ, got, w.clientWallet)
		}
	}
}

func TestNormalSide_UnknownTypeIsAnError(t *testing.T) {
	if _, err := domain.NormalSide("cash_drawer"); err == nil {
		t.Fatal("NormalSide accepted a type outside the catalogue")
	}
	if domain.IsClientOwned("cash_drawer") || domain.IsClientWallet("cash_drawer") {
		t.Fatal("an unknown type must not count as client-owned or a wallet")
	}
}

// TestBalanceDelta_FollowsTheNormalSide: a line on an account's normal
// side raises its balance, the other side lowers it (worked example
// step 1: debit provider clearing and credit the main wallet both
// raise).
func TestBalanceDelta_FollowsTheNormalSide(t *testing.T) {
	cases := []struct {
		name string
		typ  domain.AccountType
		dir  domain.Side
		want int64
	}{
		{"credit to a main wallet raises it", domain.MainWallet, domain.Credit, 1300000},
		{"debit to a main wallet lowers it", domain.MainWallet, domain.Debit, -1300000},
		{"debit to provider clearing raises it", domain.ProviderClearing, domain.Debit, 1300000},
		{"credit to provider clearing lowers it", domain.ProviderClearing, domain.Credit, -1300000},
		{"debit to a receivable raises it", domain.Receivable, domain.Debit, 1300000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.BalanceDelta(tc.typ, tc.dir, 1300000)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("BalanceDelta = %d, want %d", got, tc.want)
			}
		})
	}
	if _, err := domain.BalanceDelta(domain.MainWallet, 'X', 1); err == nil {
		t.Fatal("BalanceDelta accepted a direction other than D or C")
	}
}
