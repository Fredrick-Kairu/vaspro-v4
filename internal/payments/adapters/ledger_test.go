package adapters_test

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	ledgerv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/ledger/v1"
)

// recordingLedger is a LedgerServiceServer that records what it's sent.
type recordingLedger struct {
	ledgerv1.UnimplementedLedgerServiceServer
	mu  sync.Mutex
	got *ledgerv1.PostTransactionRequest
	txn string
}

func (r *recordingLedger) PostTransaction(_ context.Context, req *ledgerv1.PostTransactionRequest) (*ledgerv1.PostTransactionResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = req
	return &ledgerv1.PostTransactionResponse{TransactionId: r.txn}, nil
}

// TestGRPCLedger_PostTopup: each kind of top-up is a topup posting,
// keyed and referenced by its collection notice (tasks 4.4 and 4.5):
// direct (provider clearing → main wallet), into suspense (a system
// posting, no tenant), and out of suspense into the main wallet.
func TestGRPCLedger_PostTopup(t *testing.T) {
	txn := uuid.New()
	srv := &recordingLedger{txn: txn.String()}
	lis := bufconn.Listen(1 << 20)
	g := gogrpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(g, srv)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := gogrpc.NewClient("passthrough:///bufnet",
		gogrpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	l := adapters.NewGRPCLedger(conn)
	t.Cleanup(func() { _ = l.Close() })

	tenant, notice := uuid.New(), uuid.New()
	clearing := &ledgerv1.AccountRef{Type: "provider_clearing", Rail: "mpesa_ke", Currency: "KES"}
	suspense := &ledgerv1.AccountRef{Type: "suspense", Currency: "KES"}
	main := &ledgerv1.AccountRef{Type: "main_wallet", Currency: "KES"}
	for name, tc := range map[string]struct {
		kind        adapters.TopupKind
		tenant, key string
		from, to    *ledgerv1.AccountRef
	}{
		"direct":          {adapters.TopupDirect, tenant.String(), "topup:", clearing, main},
		"into suspense":   {adapters.TopupToSuspense, "", "suspense:", clearing, suspense},
		"out of suspense": {adapters.TopupFromSuspense, tenant.String(), "match:", suspense, main},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := l.PostTopup(context.Background(), adapters.TopupPosting{
				Kind: tc.kind, TenantID: tenant, NoticeID: notice, Rail: "mpesa_ke", Currency: "KES", AmountMinor: 150000, Description: "M-Pesa top-up",
			})
			if err != nil || got != txn {
				t.Fatalf("PostTopup: %v %v", got, err)
			}
			want := &ledgerv1.PostTransactionRequest{
				TenantId: tc.tenant,
				Posting: &ledgerv1.Posting{
					IdempotencyKey: tc.key + notice.String(), Type: "topup", Description: "M-Pesa top-up",
					ReferenceType: "collection_notice", ReferenceId: notice.String(),
					Lines: []*ledgerv1.Line{
						{Account: tc.from, Direction: "D", AmountMinor: 150000},
						{Account: tc.to, Direction: "C", AmountMinor: 150000},
					},
				},
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if !proto.Equal(srv.got, want) {
				t.Fatalf("sent %v, want %v", srv.got, want)
			}
		})
	}
}
