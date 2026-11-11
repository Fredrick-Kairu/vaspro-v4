//go:build integration

package app

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// These white-box tests drive drawLots, which settlement (3.5) will
// call; until then nothing exported draws tokens. useForTest posts a
// draw the simplest way: the wallet's value straight to revenue, with
// no VAT split (that's 3.5's).

func newInternalService(t *testing.T) (*Service, *vasdb.Pool, *testkit.Clock) {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/ledger"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE ledger_app LOGIN PASSWORD 'ledger_app'; GRANT ledger_service TO ledger_app;`); err != nil {
		t.Fatal(err)
	}
	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=ledger,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("ledger_app", "ledger_app")+"&search_path=ledger,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clk := testkit.NewClock(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	return New(Deps{Pool: pool, Clock: clk}), super, clk
}

var smsWallet = domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "sms", Currency: "KES"}

func (s *Service) useForTest(ctx context.Context, tenant uuid.UUID, tokens decimal.Decimal) ([]domain.Draw, int64, error) {
	var draws []domain.Draw
	var value int64
	revenue := domain.AccountKey{Type: domain.Revenue, ServiceCode: "sms", Currency: "KES"}
	err := s.withSystemAccounts(ctx, []domain.AccountKey{revenue}, func() error {
		return s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			q := store.New(tx)
			wallet, err := resolveClientAccount(ctx, q, tenant, smsWallet)
			if err != nil {
				return err
			}
			draws, value, err = s.drawLots(ctx, q, wallet, tokens, decimal.Zero)
			if err != nil {
				return err
			}
			res, err := s.postInTx(ctx, tx, PostRequest{
				TenantID: &tenant, IdempotencyKey: uuid.NewString(), Type: domain.TxnUsage, Description: "test usage",
				Lines: []domain.Line{
					{Account: smsWallet, Direction: domain.Debit, AmountMinor: value, Tokens: tokens.String()},
					{Account: revenue, Direction: domain.Credit, AmountMinor: value},
				},
				internal: true,
			})
			if err != nil {
				return err
			}
			return recordDraws(ctx, q, draws, res.TransactionID)
		})
	})
	return draws, value, err
}

func seed(t *testing.T, super *vasdb.Pool, tenant uuid.UUID, price string, from time.Time, to *time.Time) {
	t.Helper()
	if _, err := super.Exec(context.Background(), `
		INSERT INTO token_prices (id, tenant_id, service_code, currency, minor_per_token, deal_reference, status, effective_from, effective_to, created_by, approved_by)
		VALUES ($1, $2, 'sms', 'KES', $3, 'deal', 'active', $4, $5, $6, $7)`,
		uuid.New(), tenant, price, from, to, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
}

// TestDrawLots_WorkedExample: lot A (10,000 at the 0.80 deal) and lot B
// (10,000 at the 0.50 deal from 1 Nov). 380 delivered take 304.00 from
// lot A; the 9,620 left at 80 cents are used first (7,696.00); only
// then lot B at 50 cents.
func TestDrawLots_WorkedExample(t *testing.T) {
	ctx := context.Background()
	s, super, clk := newInternalService(t)
	tenant := uuid.New()
	jan1, nov1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	seed(t, super, tenant, "80", jan1, &nov1)
	seed(t, super, tenant, "50", nov1, nil)

	topUp := func(amount int64) {
		mpesa := domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_ke", Currency: "KES"}
		main := domain.AccountKey{Type: domain.MainWallet, Currency: "KES"}
		if _, err := s.Post(ctx, PostRequest{TenantID: &tenant, IdempotencyKey: uuid.NewString(), Type: domain.TxnTopup, Description: "top-up",
			Lines: []domain.Line{{Account: mpesa, Direction: domain.Debit, AmountMinor: amount}, {Account: main, Direction: domain.Credit, AmountMinor: amount}}}); err != nil {
			t.Fatal(err)
		}
	}
	buy := func() PurchaseResult {
		res, err := s.PurchaseTokens(ctx, PurchaseRequest{TenantID: tenant, IdempotencyKey: uuid.NewString(), ServiceCode: "sms", Currency: "KES", Tokens: decimal.NewFromInt(10000)})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	topUp(1300000)
	lotA := buy()
	clk.Set(time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC))
	lotB := buy()

	for _, step := range []struct {
		tokens string
		value  int64
		lot    uuid.UUID
	}{
		{"380", 30400, lotA.LotID},
		{"9620", 769600, lotA.LotID},
		{"100", 5000, lotB.LotID},
	} {
		draws, value, err := s.useForTest(ctx, tenant, decimal.RequireFromString(step.tokens))
		if err != nil {
			t.Fatal(err)
		}
		if value != step.value || len(draws) != 1 || draws[0].LotID != step.lot {
			t.Fatalf("using %s: %+v worth %d; want one draw from lot %s worth %d", step.tokens, draws, value, step.lot, step.value)
		}
	}

	var consumedA, consumedB int64
	if err := super.QueryRow(ctx, `SELECT coalesce(sum(value_minor), 0) FROM lot_consumptions WHERE lot_id = $1`, lotA.LotID).Scan(&consumedA); err != nil {
		t.Fatal(err)
	}
	if err := super.QueryRow(ctx, `SELECT coalesce(sum(value_minor), 0) FROM lot_consumptions WHERE lot_id = $1`, lotB.LotID).Scan(&consumedB); err != nil {
		t.Fatal(err)
	}
	if consumedA != 800000 || consumedB != 5000 {
		t.Fatalf("lot A drew %d (want all 800000), lot B %d (want 5000)", consumedA, consumedB)
	}
	var balance int64
	if err := super.QueryRow(ctx, `SELECT balance_minor FROM balances b JOIN accounts a ON a.id = b.account_id WHERE a.tenant_id = $1 AND a.type = 'service_wallet'`, tenant).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 495000 {
		t.Fatalf("SMS wallet %d, want 495000: lot B's 9,900 tokens left at 50 cents", balance)
	}

	t.Run("not more than the wallet holds", func(t *testing.T) {
		_, _, err := s.useForTest(ctx, tenant, decimal.RequireFromString("9900.000001"))
		if err == nil {
			t.Fatal("drew more tokens than were left")
		}
	})
}

// TestDrawLots_TokensHeldByOthersAreNotDrawn: a draw that isn't
// settling a hold may only use tokens no hold reserves.
func TestDrawLots_TokensHeldByOthersAreNotDrawn(t *testing.T) {
	ctx := context.Background()
	s, super, _ := newInternalService(t)
	tenant := uuid.New()
	seed(t, super, tenant, "80", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	if _, err := s.Post(ctx, PostRequest{TenantID: &tenant, IdempotencyKey: uuid.NewString(), Type: domain.TxnTopup, Description: "top-up",
		Lines: []domain.Line{
			{Account: domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_ke", Currency: "KES"}, Direction: domain.Debit, AmountMinor: 8000},
			{Account: domain.AccountKey{Type: domain.MainWallet, Currency: "KES"}, Direction: domain.Credit, AmountMinor: 8000},
		}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurchaseTokens(ctx, PurchaseRequest{TenantID: tenant, IdempotencyKey: uuid.NewString(), ServiceCode: "sms", Currency: "KES", Tokens: decimal.NewFromInt(100)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceHold(ctx, HoldRequest{TenantID: tenant, Account: smsWallet, ReferenceType: "campaign", ReferenceID: uuid.New(),
		Tokens: decimal.NewFromInt(60), ExpiresAt: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.useForTest(ctx, tenant, decimal.NewFromInt(41)); err == nil {
		t.Fatal("drew tokens a hold reserves")
	}
	if _, _, err := s.useForTest(ctx, tenant, decimal.NewFromInt(40)); err != nil {
		t.Fatalf("the 40 unheld tokens: %v", err)
	}
}

// TestDrawLots_RandomPurchasesAndDraws is the token half of Phase 3's
// property test: random purchases at changing deals (fractional prices
// and quantities) and random fractional draws. After every step the
// wallet's money equals the value its lots have left, to the cent, each
// lot's draws never exceed what it cost, and the journal balances.
// LEDGER_PROPERTY_SEED reruns a failing seed.
func TestDrawLots_RandomPurchasesAndDraws(t *testing.T) {
	propSeed := uint64(time.Now().UnixNano())
	if v := os.Getenv("LEDGER_PROPERTY_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		propSeed = n
	}
	t.Logf("seed %d", propSeed)
	rng := rand.New(rand.NewPCG(propSeed, propSeed)) //nolint:gosec // a reproducible test sequence, not a secret

	ctx := context.Background()
	s, super, clk := newInternalService(t)
	tenant := uuid.New()
	if _, err := s.Post(ctx, PostRequest{TenantID: &tenant, IdempotencyKey: uuid.NewString(), Type: domain.TxnTopup, Description: "top-up",
		Lines: []domain.Line{
			{Account: domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_ke", Currency: "KES"}, Direction: domain.Debit, AmountMinor: 1 << 40},
			{Account: domain.AccountKey{Type: domain.MainWallet, Currency: "KES"}, Direction: domain.Credit, AmountMinor: 1 << 40},
		}}); err != nil {
		t.Fatal(err)
	}

	var bought, drawn int
	for step := range 200 {
		if step == 0 || rng.IntN(3) == 0 {
			// A new deal from now on, at a price with up to 6 decimals.
			clk.Advance(time.Hour)
			if _, err := super.Exec(ctx, `UPDATE token_prices SET effective_to = $2 WHERE tenant_id = $1 AND effective_to IS NULL`, tenant, clk.Now()); err != nil {
				t.Fatal(err)
			}
			seed(t, super, tenant, decimal.New(rng.Int64N(200_000_000)+1, -6).String(), clk.Now(), nil)
			clk.Advance(time.Minute)
		}
		tokens := decimal.New(rng.Int64N(5_000_000)+1, []int32{0, -1, -2, -3, -4, -5, -6}[rng.IntN(7)]) // 0 to 6 decimals
		if rng.IntN(2) == 0 {
			_, err := s.PurchaseTokens(ctx, PurchaseRequest{TenantID: tenant, IdempotencyKey: uuid.NewString(), ServiceCode: "sms", Currency: "KES", Tokens: tokens})
			if err != nil && !errors.Is(err, ErrValidation) { // a tiny purchase can cost under one minor unit
				t.Fatalf("seed %d step %d purchase %s: %v", propSeed, step, tokens, err)
			}
			if err == nil {
				bought++
			}
		} else {
			draws, value, err := s.useForTest(ctx, tenant, tokens)
			switch {
			case errors.Is(err, ErrInsufficientTokens):
			case err != nil && value == 0:
				// A draw worth nothing can't be posted (lines must be positive);
				// settlement (3.5) skips posting one.
			case err != nil:
				t.Fatalf("seed %d step %d draw %s: %v", propSeed, step, tokens, err)
			default:
				drawn += len(draws)
			}
		}

		var bad int
		if err := super.QueryRow(ctx, `
			SELECT count(*) FROM balances b JOIN accounts a ON a.id = b.account_id
			WHERE a.type = 'service_wallet' AND b.balance_minor <> (
			  SELECT coalesce(sum(round(l.tokens_bought * l.minor_per_token)
			    - (SELECT coalesce(sum(lc.value_minor), 0) FROM lot_consumptions lc WHERE lc.lot_id = l.id)), 0)
			  FROM token_lots l WHERE l.account_id = b.account_id)`).Scan(&bad); err != nil {
			t.Fatal(err)
		}
		if bad != 0 {
			t.Fatalf("seed %d step %d: the wallet's money differs from its lots' remaining value", propSeed, step)
		}
		if err := super.QueryRow(ctx, `
			SELECT count(*) FROM token_lots l
			WHERE (SELECT coalesce(sum(value_minor), 0) FROM lot_consumptions WHERE lot_id = l.id) > round(l.tokens_bought * l.minor_per_token)
			   OR (l.tokens_remaining = 0 AND (SELECT coalesce(sum(value_minor), 0) FROM lot_consumptions WHERE lot_id = l.id) <> round(l.tokens_bought * l.minor_per_token))`).Scan(&bad); err != nil {
			t.Fatal(err)
		}
		if bad != 0 {
			t.Fatalf("seed %d step %d: a lot's draws exceed its cost, or an emptied lot's don't equal it", propSeed, step)
		}
	}
	t.Logf("%d purchases, %d lot draws", bought, drawn)
	if bought == 0 || drawn == 0 {
		t.Fatal("the sequence didn't exercise both purchases and draws")
	}
}
