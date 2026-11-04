//go:build integration

package grpc_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	ledgergrpc "github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/grpc"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	ledgerv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/ledger/v1"
)

var now = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

// newClient serves the ledger's gRPC API over an in-memory listener,
// backed by a real app.Service on real Postgres as ledger_app.
func newClient(t *testing.T) ledgerv1.LedgerServiceClient {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/ledger"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE ledger_app LOGIN PASSWORD 'ledger_app'; GRANT ledger_service TO ledger_app;`); err != nil {
		t.Fatal(err)
	}
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("ledger_app", "ledger_app")+"&search_path=ledger,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	svc := app.New(app.Deps{Pool: pool, Clock: testkit.NewClock(now)})
	// The SMS list price, KES 0.80 (staff entry is 3.4b's API).
	if _, err := pg.Pool.Exec(ctx, `
		INSERT INTO ledger.token_prices (id, service_code, currency, minor_per_token, status, effective_from, created_by, approved_by)
		VALUES ($1, 'sms', 'KES', 80, 'active', '2026-01-01', $2, $3)`, uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}

	lis := bufconn.Listen(1 << 20)
	srv := gogrpc.NewServer()
	ledgergrpc.Register(srv, svc, nil)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := gogrpc.NewClient("passthrough:///bufnet",
		gogrpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return ledgerv1.NewLedgerServiceClient(conn)
}

// wantStatus checks the gRPC code and, when reason isn't empty, the
// ErrorInfo detail carrying the VASPRO code.
func wantStatus(t *testing.T, err error, code codes.Code, reason string) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != code {
		t.Fatalf("got %v, want %s", err, code)
	}
	if reason == "" {
		return
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			if info.GetReason() != reason || info.GetDomain() != "ledger.vaspro.io" {
				t.Fatalf("ErrorInfo %s/%s, want %s", info.GetDomain(), info.GetReason(), reason)
			}
			return
		}
	}
	t.Fatalf("%v carries no ErrorInfo, want reason %s", err, reason)
}

var (
	mainKES  = &ledgerv1.AccountRef{Type: "main_wallet", Currency: "KES"}
	smsKES   = &ledgerv1.AccountRef{Type: "service_wallet", ServiceCode: "sms", Currency: "KES"}
	mpesaKES = &ledgerv1.AccountRef{Type: "provider_clearing", Rail: "mpesa_ke", Currency: "KES"}
)

func line(acct *ledgerv1.AccountRef, dir string, amount int64) *ledgerv1.Line {
	return &ledgerv1.Line{Account: acct, Direction: dir, AmountMinor: amount}
}

func posting(typ string, lines ...*ledgerv1.Line) *ledgerv1.Posting {
	return &ledgerv1.Posting{IdempotencyKey: uuid.NewString(), Type: typ, Description: "test", Lines: lines}
}

func TestLedgerService_PostHoldCaptureRelease(t *testing.T) {
	client := newClient(t)
	ctx := context.Background()
	tenant := uuid.NewString()

	topup := &ledgerv1.PostTransactionRequest{TenantId: tenant, Posting: posting("topup", line(mpesaKES, "D", 1300000), line(mainKES, "C", 1300000))}
	res, err := client.PostTransaction(ctx, topup)
	if err != nil {
		t.Fatal(err)
	}
	if res.GetReplayed() || len(res.GetLines()) != 2 || res.GetLines()[0].GetAccountId() == "" {
		t.Fatalf("top-up: %+v", res)
	}
	again, err := client.PostTransaction(ctx, topup)
	if err != nil || !again.GetReplayed() || again.GetTransactionId() != res.GetTransactionId() {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	bought, err := client.PurchaseTokens(ctx, &ledgerv1.PurchaseTokensRequest{TenantId: tenant, IdempotencyKey: uuid.NewString(),
		ServiceCode: "sms", Currency: "KES", Tokens: "10000"})
	if err != nil || bought.GetCostMinor() != 800000 || bought.GetMinorPerToken() != "80" || bought.GetLotId() == "" {
		t.Fatalf("purchase: %+v, %v", bought, err)
	}

	ref := uuid.NewString()
	hold, err := client.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{TenantId: tenant, Account: smsKES,
		ReferenceType: "campaign", ReferenceId: ref, Tokens: "400", ExpiresAtUnix: now.Add(time.Hour).Unix()})
	if err != nil || hold.GetHold().GetStatus() != "active" || hold.GetHold().GetTokens() != "400" || hold.GetHold().GetAmountMinor() != 32000 {
		t.Fatalf("hold: %+v, %v", hold, err)
	}
	captured, err := client.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{TenantId: tenant, ReferenceType: "campaign", ReferenceId: ref, CapturedTokens: "380"})
	if err != nil || captured.GetHold().GetStatus() != "captured" || captured.GetHold().GetCapturedTokens() != "380" || captured.GetHold().GetSettled() {
		t.Fatalf("capture: %+v, %v", captured, err)
	}

	bals, err := client.GetBalances(ctx, &ledgerv1.GetBalancesRequest{TenantId: tenant})
	if err != nil {
		t.Fatal(err)
	}
	var sms *ledgerv1.AccountBalance
	for _, b := range bals.GetAccounts() {
		if b.GetAccount().GetType() == "service_wallet" {
			sms = b
		}
	}
	if sms == nil || sms.GetBalanceMinor() != 800000 || sms.GetHeldTokens() != "380" || sms.GetTokensAvailable() != "9620" || sms.GetTokensRemaining() != "10000" {
		t.Fatalf("SMS wallet %+v", sms)
	}

	other := uuid.NewString()
	if _, err := client.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{TenantId: tenant, Account: smsKES,
		ReferenceType: "campaign", ReferenceId: other, Tokens: "1.5", ExpiresAtUnix: now.Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	released, err := client.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{TenantId: tenant, ReferenceType: "campaign", ReferenceId: other})
	if err != nil || released.GetHold().GetStatus() != "released" {
		t.Fatalf("release: %+v, %v", released, err)
	}

	system, err := client.GetBalances(ctx, &ledgerv1.GetBalancesRequest{})
	if err != nil || len(system.GetAccounts()) != 1 || system.GetAccounts()[0].GetBalanceMinor() != 1300000 {
		t.Fatalf("VASPRO's balances: %+v, %v", system, err)
	}
}

func TestLedgerService_Errors(t *testing.T) {
	client := newClient(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	future := now.Add(time.Hour).Unix()

	topup := &ledgerv1.PostTransactionRequest{TenantId: tenant, Posting: posting("topup", line(mpesaKES, "D", 1000), line(mainKES, "C", 1000))}
	if _, err := client.PostTransaction(ctx, topup); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		call   func() error
		code   codes.Code
		reason string
	}{
		{"insufficient balance", func() error {
			_, err := client.PurchaseTokens(ctx, &ledgerv1.PurchaseTokensRequest{TenantId: tenant, IdempotencyKey: uuid.NewString(),
				ServiceCode: "sms", Currency: "KES", Tokens: "13"}) // 10.40 against 10.00
			return err
		}, codes.FailedPrecondition, "BAL-4001"},
		{"no price for the service", func() error {
			_, err := client.PurchaseTokens(ctx, &ledgerv1.PurchaseTokensRequest{TenantId: tenant, IdempotencyKey: uuid.NewString(),
				ServiceCode: "whatsapp", Currency: "KES", Tokens: "1"})
			return err
		}, codes.FailedPrecondition, "BAL-4005"},
		{"tokens that aren't a decimal", func() error {
			_, err := client.PurchaseTokens(ctx, &ledgerv1.PurchaseTokensRequest{TenantId: tenant, IdempotencyKey: uuid.NewString(),
				ServiceCode: "sms", Currency: "KES", Tokens: "1e3"})
			return err
		}, codes.InvalidArgument, "VAL-4000"},
		{"a service wallet moves only through tokens", func() error {
			_, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{TenantId: tenant,
				Posting: posting("token_purchase", line(mainKES, "D", 1), line(smsKES, "C", 1))})
			return err
		}, codes.InvalidArgument, "VAL-4000"},
		{"allocation is final", func() error {
			bonus := &ledgerv1.AccountRef{Type: "bonus", Currency: "KES"}
			_, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{TenantId: tenant,
				Posting: posting("refund", line(bonus, "D", 1), line(mainKES, "C", 1))})
			return err
		}, codes.InvalidArgument, "BAL-4004"},
		{"key reused for another posting", func() error {
			p := posting("topup", line(mpesaKES, "D", 5), line(mainKES, "C", 5))
			p.IdempotencyKey = topup.GetPosting().GetIdempotencyKey()
			_, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{TenantId: tenant, Posting: p})
			return err
		}, codes.AlreadyExists, "VAL-4001"},
		{"unbalanced", func() error {
			_, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{TenantId: tenant,
				Posting: posting("topup", line(mpesaKES, "D", 5), line(mainKES, "C", 4))})
			return err
		}, codes.InvalidArgument, "VAL-4000"},
		{"a direction that isn't one letter", func() error {
			_, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{TenantId: tenant,
				Posting: posting("topup", line(mpesaKES, "Debit", 5), line(mainKES, "C", 5))})
			return err
		}, codes.InvalidArgument, "VAL-4000"},
		{"no posting", func() error {
			_, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{TenantId: tenant})
			return err
		}, codes.InvalidArgument, "VAL-4000"},
		{"a malformed tenant", func() error {
			_, err := client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{TenantId: "acme", Posting: topup.GetPosting()})
			return err
		}, codes.InvalidArgument, "VAL-4000"},
		{"a hold beyond the balance", func() error {
			_, err := client.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{TenantId: tenant, Account: mainKES,
				ReferenceType: "payout", ReferenceId: uuid.NewString(), AmountMinor: 1001, ExpiresAtUnix: future})
			return err
		}, codes.FailedPrecondition, "BAL-4001"},
		{"a hold with no tenant", func() error {
			_, err := client.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{Account: mainKES,
				ReferenceType: "payout", ReferenceId: uuid.NewString(), AmountMinor: 1, ExpiresAtUnix: future})
			return err
		}, codes.InvalidArgument, "VAL-4000"},
		{"releasing an unknown hold", func() error {
			_, err := client.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{TenantId: tenant, ReferenceType: "payout", ReferenceId: uuid.NewString()})
			return err
		}, codes.NotFound, "VAL-4003"},
		{"a malformed balances tenant", func() error {
			_, err := client.GetBalances(ctx, &ledgerv1.GetBalancesRequest{TenantId: "x"})
			return err
		}, codes.InvalidArgument, "VAL-4000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { wantStatus(t, tc.call(), tc.code, tc.reason) })
	}

	t.Run("hold state", func(t *testing.T) {
		if _, err := client.PurchaseTokens(ctx, &ledgerv1.PurchaseTokensRequest{TenantId: tenant, IdempotencyKey: uuid.NewString(),
			ServiceCode: "sms", Currency: "KES", Tokens: "10"}); err != nil {
			t.Fatal(err)
		}
		ref := uuid.NewString()
		if _, err := client.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{TenantId: tenant, Account: smsKES,
			ReferenceType: "campaign", ReferenceId: ref, Tokens: "5", ExpiresAtUnix: future}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{TenantId: tenant, ReferenceType: "campaign", ReferenceId: ref}); err != nil {
			t.Fatal(err)
		}
		_, err := client.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{TenantId: tenant, ReferenceType: "campaign", ReferenceId: ref, CapturedTokens: "5"})
		wantStatus(t, err, codes.FailedPrecondition, "BAL-4003")
	})

	t.Run("a money hold is captured with its posting", func(t *testing.T) {
		ref := uuid.NewString()
		if _, err := client.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{TenantId: tenant, Account: mainKES,
			ReferenceType: "payout", ReferenceId: ref, AmountMinor: 10, ExpiresAtUnix: future}); err != nil {
			t.Fatal(err)
		}
		_, err := client.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{TenantId: tenant, ReferenceType: "payout", ReferenceId: ref, CapturedMinor: 10})
		wantStatus(t, err, codes.InvalidArgument, "VAL-4000")
	})
}
