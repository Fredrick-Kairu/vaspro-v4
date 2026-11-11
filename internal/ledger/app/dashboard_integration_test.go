//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// member is a dashboard user granted perms in tenant (task 3.9).
func member(h harness, tenant uuid.UUID, perms ...string) uuid.UUID {
	user := uuid.New()
	h.access.Grant(user, tenant, perms...)
	return user
}

func statement(t *testing.T, h harness, user, tenant uuid.UUID, q app.StatementQuery) app.Statement {
	t.Helper()
	st, err := h.svc.Statement(context.Background(), user, tenant, q)
	if err != nil {
		t.Fatalf("Statement: %v", err)
	}
	return st
}

type wantLine struct {
	typ          domain.TxnType
	dir          domain.Side
	amount       int64
	tokens       string // "" for a money line
	after        int64
	tokensAfter  string // "" for a main wallet
	referenceSet bool
}

func checkLines(t *testing.T, got []app.StatementLine, want []wantLine) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d lines, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Type != w.typ || g.Direction != w.dir || g.AmountMinor != w.amount || g.BalanceAfterMinor != w.after {
			t.Fatalf("line %d: %s %c %d after %d, want %s %c %d after %d", i, g.Type, g.Direction, g.AmountMinor, g.BalanceAfterMinor, w.typ, w.dir, w.amount, w.after)
		}
		if (g.Tokens == nil) != (w.tokens == "") || (g.Tokens != nil && !g.Tokens.Equal(tok(w.tokens))) {
			t.Fatalf("line %d: tokens %v, want %q", i, g.Tokens, w.tokens)
		}
		if (g.TokensAfter == nil) != (w.tokensAfter == "") || (g.TokensAfter != nil && !g.TokensAfter.Equal(tok(w.tokensAfter))) {
			t.Fatalf("line %d: tokens after %v, want %q", i, g.TokensAfter, w.tokensAfter)
		}
	}
}

// TestStatement_WorkedExample: the worked example's wallets as the
// dashboard shows them, newest first, each line with the balance after
// it.
func TestStatement_WorkedExample(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	finance := member(h, tenant, app.PermBillingRead, app.PermBillingManage)
	topUp(t, h, tenant, 1_300_000)
	if _, err := h.svc.BuyTokens(ctx, finance, purchase(tenant, "sms", "KES", "10000")); err != nil {
		t.Fatal(err)
	}
	ref := uuid.New()
	if _, err := h.svc.PlaceHold(ctx, app.HoldRequest{TenantID: tenant, Account: smsKES, ReferenceType: "campaign", ReferenceID: ref,
		Tokens: tok("400"), ExpiresAt: testNow.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: "campaign", ReferenceID: ref, CapturedTokens: tok("380")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SettleUsage(ctx, testNow.Add(app.DefaultSettlementWindow)); err != nil {
		t.Fatal(err)
	}
	main := balanceOf(t, h, tenant, mainKES).AccountID
	sms := balanceOf(t, h, tenant, smsKES).AccountID

	st := statement(t, h, finance, tenant, app.StatementQuery{AccountID: main})
	if st.Account != mainKES {
		t.Fatalf("account %+v", st.Account)
	}
	checkLines(t, st.Lines, []wantLine{
		{typ: domain.TxnTokenPurchase, dir: domain.Debit, amount: 800_000, after: 500_000},
		{typ: domain.TxnTopup, dir: domain.Credit, amount: 1_300_000, after: 1_300_000},
	})
	checkLines(t, statement(t, h, finance, tenant, app.StatementQuery{AccountID: sms}).Lines, []wantLine{
		{typ: domain.TxnUsage, dir: domain.Debit, amount: 30_400, tokens: "380", after: 769_600, tokensAfter: "9620"},
		{typ: domain.TxnTokenPurchase, dir: domain.Credit, amount: 800_000, tokens: "10000", after: 800_000, tokensAfter: "10000"},
	})

	t.Run("paged one line at a time", func(t *testing.T) {
		first := statement(t, h, finance, tenant, app.StatementQuery{AccountID: main, Limit: 1})
		checkLines(t, first.Lines, []wantLine{{typ: domain.TxnTokenPurchase, dir: domain.Debit, amount: 800_000, after: 500_000}})
		if first.NextCursor == "" {
			t.Fatal("no cursor after the first page")
		}
		second := statement(t, h, finance, tenant, app.StatementQuery{AccountID: main, Limit: 1, Cursor: first.NextCursor})
		checkLines(t, second.Lines, []wantLine{{typ: domain.TxnTopup, dir: domain.Credit, amount: 1_300_000, after: 1_300_000}})
		if second.NextCursor != "" {
			t.Fatalf("a cursor after the last line: %q", second.NextCursor)
		}
	})

	t.Run("a date range", func(t *testing.T) {
		// posted_at is the database's time, not the test clock's.
		past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
		if n := len(statement(t, h, finance, tenant, app.StatementQuery{AccountID: main, From: &past, To: &future}).Lines); n != 2 {
			t.Fatalf("%d lines in range, want 2", n)
		}
		if n := len(statement(t, h, finance, tenant, app.StatementQuery{AccountID: main, From: &future}).Lines); n != 0 {
			t.Fatalf("%d lines from the future, want 0", n)
		}
		if n := len(statement(t, h, finance, tenant, app.StatementQuery{AccountID: main, To: &past}).Lines); n != 0 {
			t.Fatalf("%d lines before they were posted, want 0", n)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		other := uuid.New()
		otherMain := topUp(t, h, other, 100).Lines
		var otherAccount uuid.UUID
		for _, l := range otherMain {
			if l.Line.Account == mainKES {
				otherAccount = l.AccountID
			}
		}
		mpesa := balanceIDOfSystem(t, h, mpesaKES)
		for name, id := range map[string]uuid.UUID{"another tenant's account": otherAccount, "VASPRO's account": mpesa, "an unknown account": uuid.New()} {
			_, err := h.svc.Statement(ctx, finance, tenant, app.StatementQuery{AccountID: id})
			if !errors.Is(err, app.ErrNotFound) {
				t.Fatalf("%s: got %v, want ErrNotFound", name, err)
			}
		}
		_, err := h.svc.Statement(ctx, finance, tenant, app.StatementQuery{AccountID: main, Cursor: "!!"})
		wantErr(t, err, app.ErrValidation)
		_, err = h.svc.Statement(ctx, finance, tenant, app.StatementQuery{AccountID: main, Limit: 101})
		wantErr(t, err, app.ErrValidation)
	})
}

func balanceIDOfSystem(t *testing.T, h harness, key domain.AccountKey) uuid.UUID {
	t.Helper()
	bals, err := h.svc.GetBalances(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bals {
		if b.Account == key {
			return b.AccountID
		}
	}
	t.Fatalf("no VASPRO %+v account", key)
	return uuid.Nil
}

// TestStatement_ConcurrentPostings: while top-ups land, every page read
// still adds up, and the newest line's balance is a balance the wallet
// really had.
func TestStatement_ConcurrentPostings(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	reader := member(h, tenant, app.PermBillingRead)
	topUp(t, h, tenant, 1_000)
	main := balanceOf(t, h, tenant, mainKES).AccountID

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 40 {
			if _, err := h.svc.Post(ctx, post(tenant, domain.TxnTopup, d(mpesaKES, 7), c(mainKES, 7))); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	// Read while the top-ups land; any broken page is reported once the
	// writer has stopped.
	var broken string
	for range 30 {
		st, err := h.svc.Statement(ctx, reader, tenant, app.StatementQuery{AccountID: main, Limit: 10})
		if err != nil {
			broken = err.Error()
			break
		}
		for i := 1; i < len(st.Lines) && broken == ""; i++ {
			if st.Lines[i-1].BalanceAfterMinor-st.Lines[i-1].AmountMinor != st.Lines[i].BalanceAfterMinor {
				broken = fmt.Sprintf("line %d after %d less its %d isn't the next line's %d", i-1, st.Lines[i-1].BalanceAfterMinor, st.Lines[i-1].AmountMinor, st.Lines[i].BalanceAfterMinor)
			}
		}
		if top := st.Lines[0].BalanceAfterMinor; broken == "" && (top-1_000)%7 != 0 {
			broken = fmt.Sprintf("newest balance %d isn't 1,000 plus a number of 7s", top)
		}
		if broken != "" {
			break
		}
	}
	wg.Wait()
	if broken != "" {
		t.Fatal(broken)
	}
	st := statement(t, h, reader, tenant, app.StatementQuery{AccountID: main, Limit: 100})
	if len(st.Lines) != 41 || st.Lines[0].BalanceAfterMinor != 1_280 || st.Lines[40].BalanceAfterMinor != 1_000 {
		t.Fatalf("after the top-ups: %d lines, newest %d, oldest %d", len(st.Lines), st.Lines[0].BalanceAfterMinor, st.Lines[len(st.Lines)-1].BalanceAfterMinor)
	}
}

// TestTokenBuying_Dashboard: the prices a client sees, a quote, and the
// purchase that follows it at the same price and cost.
func TestTokenBuying_Dashboard(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	finance := member(h, tenant, app.PermBillingRead, app.PermBillingManage)
	seedPrice(t, h, &tenant, "sms", "KES", "0", "50", jan1, nil)
	seedPrice(t, h, &tenant, "sms", "KES", "50000", "45", jan1, nil)
	topUp(t, h, tenant, 10_000)

	prices, err := h.svc.TenantPrices(ctx, finance, tenant)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, p := range prices {
		got[p.ServiceCode+"/"+p.Currency] = append(got[p.ServiceCode+"/"+p.Currency], p.Source+":"+p.MinTokens.String()+"@"+p.MinorPerToken.String())
	}
	if want := []string{"deal:0@50", "deal:50000@45"}; len(got["sms/KES"]) != 2 || got["sms/KES"][0] != want[0] || got["sms/KES"][1] != want[1] {
		t.Fatalf("KES prices %v, want the deal's two tiers and not the list's", got["sms/KES"])
	}
	if want := "list:0@12.5"; len(got["sms/UGX"]) != 1 || got["sms/UGX"][0] != want {
		t.Fatalf("UGX prices %v, want the list's", got["sms/UGX"])
	}

	quote, err := h.svc.QuoteTokens(ctx, finance, tenant, "sms", "KES", tok("100.5"))
	if err != nil {
		t.Fatal(err)
	}
	if quote.Source != "deal" || !quote.MinorPerToken.Equal(tok("50")) || quote.CostMinor != 5_025 || quote.MainAvailableMinor != 10_000 || !quote.Affordable {
		t.Fatalf("quote %+v", quote)
	}
	bought, err := h.svc.BuyTokens(ctx, finance, purchase(tenant, "sms", "KES", "100.5"))
	if err != nil {
		t.Fatal(err)
	}
	if bought.CostMinor != quote.CostMinor || !bought.MinorPerToken.Equal(quote.MinorPerToken) {
		t.Fatalf("bought at %s for %d, quoted %s for %d", bought.MinorPerToken, bought.CostMinor, quote.MinorPerToken, quote.CostMinor)
	}

	short, err := h.svc.QuoteTokens(ctx, finance, tenant, "sms", "KES", tok("1000"))
	if err != nil || short.Affordable || short.CostMinor != 50_000 {
		t.Fatalf("an unaffordable quote: %+v, %v", short, err)
	}
	_, err = h.svc.QuoteTokens(ctx, finance, tenant, "whatsapp", "KES", tok("1"))
	wantErr(t, err, app.ErrNoPrice)
	_, err = h.svc.QuoteTokens(ctx, finance, tenant, "sms", "KES", tok("-1"))
	wantErr(t, err, app.ErrValidation)

	t.Run("a purchase repeated with its key", func(t *testing.T) {
		req := purchase(tenant, "sms", "KES", "10")
		first, err := h.svc.BuyTokens(ctx, finance, req)
		if err != nil {
			t.Fatal(err)
		}
		again, err := h.svc.BuyTokens(ctx, finance, req)
		if err != nil || !again.Replayed || again.LotID != first.LotID {
			t.Fatalf("replay: %+v, %v", again, err)
		}
	})

	t.Run("auto-buy rules", func(t *testing.T) {
		rule, err := h.svc.SetTenantAutoBuyRule(ctx, finance, tenant, app.AutoBuyRule{ServiceCode: "sms", Currency: "KES", BelowTokens: tok("100"), BuyTokens: tok("1000"), Enabled: true})
		if err != nil || !rule.BuyTokens.Equal(tok("1000")) {
			t.Fatalf("set: %+v, %v", rule, err)
		}
		rules, err := h.svc.TenantAutoBuyRules(ctx, finance, tenant)
		if err != nil || len(rules) != 1 || rules[0].ID != rule.ID {
			t.Fatalf("list: %+v, %v", rules, err)
		}
	})
}

// TestDashboard_Permissions: billing:read to look, billing:manage to
// buy or change rules; anyone else's account doesn't exist; accounts
// down fails closed.
func TestDashboard_Permissions(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	topUp(t, h, tenant, 100_000)
	main := balanceOf(t, h, tenant, mainKES).AccountID
	viewer := member(h, tenant, app.PermBillingRead)
	developer := member(h, tenant) // in the account, without billing permissions
	stranger := uuid.New()

	reads := map[string]func(user uuid.UUID) error{
		"balances": func(u uuid.UUID) error { _, err := h.svc.TenantBalances(ctx, u, tenant); return err },
		"statement": func(u uuid.UUID) error {
			_, err := h.svc.Statement(ctx, u, tenant, app.StatementQuery{AccountID: main})
			return err
		},
		"prices": func(u uuid.UUID) error { _, err := h.svc.TenantPrices(ctx, u, tenant); return err },
		"quote": func(u uuid.UUID) error {
			_, err := h.svc.QuoteTokens(ctx, u, tenant, "sms", "KES", tok("1"))
			return err
		},
		"rules": func(u uuid.UUID) error { _, err := h.svc.TenantAutoBuyRules(ctx, u, tenant); return err },
	}
	writes := map[string]func(user uuid.UUID) error{
		"purchase": func(u uuid.UUID) error {
			_, err := h.svc.BuyTokens(ctx, u, purchase(tenant, "sms", "KES", "1"))
			return err
		},
		"rule": func(u uuid.UUID) error {
			_, err := h.svc.SetTenantAutoBuyRule(ctx, u, tenant, app.AutoBuyRule{ServiceCode: "sms", Currency: "KES", BelowTokens: tok("1"), BuyTokens: tok("2"), Enabled: true})
			return err
		},
	}
	for name, call := range reads {
		if err := call(viewer); err != nil {
			t.Fatalf("viewer %s: %v", name, err)
		}
		wantErr(t, call(developer), app.ErrPermissionDenied)
		wantErr(t, call(stranger), app.ErrNotFound)
	}
	for name, call := range writes {
		if err := call(viewer); !errors.Is(err, app.ErrPermissionDenied) {
			t.Fatalf("viewer %s: got %v, want ErrPermissionDenied", name, err)
		}
		wantErr(t, call(stranger), app.ErrNotFound)
	}
	if got := count(t, h, `SELECT count(*) FROM token_lots`); got != 0 {
		t.Fatalf("refused purchases wrote %d lots", got)
	}

	h.access.Revoke(viewer, tenant)
	wantErr(t, reads["balances"](viewer), app.ErrNotFound)

	h.access.Fail()
	if err := reads["balances"](developer); err == nil || errors.Is(err, app.ErrNotFound) || errors.Is(err, app.ErrPermissionDenied) {
		t.Fatalf("accounts down: got %v, want an error that fails closed", err)
	}
}

// TestDevTopUp: staff credit a main wallet from the sandbox rail, only
// where LEDGER_DEV_TOPUPS is on (task 3.9, OQ-20).
func TestDevTopUp(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	staff := newStaff(h)
	tenant := uuid.New()
	req := app.DevTopUpRequest{TenantID: tenant, Currency: "KES", AmountMinor: 500_000, IdempotencyKey: "dev-1"}

	_, err := h.svc.DevTopUp(ctx, staff.user, req)
	wantErr(t, err, app.ErrNotFound)
	if got := count(t, h, `SELECT count(*) FROM transactions`); got != 0 {
		t.Fatalf("a dev top-up with the flag off wrote %d transactions", got)
	}

	dev := h.service(t, true)
	_, err = dev.DevTopUp(ctx, uuid.New(), req)
	wantErr(t, err, app.ErrNotStaff)
	res, err := dev.DevTopUp(ctx, staff.user, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, h, tenant, mainKES).BalanceMinor; got != 500_000 {
		t.Fatalf("main wallet %d, want 500,000", got)
	}
	sandbox := domain.AccountKey{Type: domain.ProviderClearing, Rail: "sandbox", Currency: "KES"}
	if got := systemBalance(t, h, sandbox); got != 500_000 {
		t.Fatalf("sandbox clearing %d, want 500,000", got)
	}
	assertEventValid(t, h, "wallet.credited", balanceOf(t, h, tenant, mainKES).AccountID)
	again, err := dev.DevTopUp(ctx, staff.user, req)
	if err != nil || !again.Replayed || again.TransactionID != res.TransactionID {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	for name, bad := range map[string]app.DevTopUpRequest{
		"zero":             {TenantID: tenant, Currency: "KES", AmountMinor: 0, IdempotencyKey: "dev-2"},
		"no key":           {TenantID: tenant, Currency: "KES", AmountMinor: 1},
		"unknown currency": {TenantID: tenant, Currency: "XXX", AmountMinor: 1, IdempotencyKey: "dev-3"},
	} {
		if _, err := dev.DevTopUp(ctx, staff.user, bad); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
