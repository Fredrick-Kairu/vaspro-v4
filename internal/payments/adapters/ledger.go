package adapters

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ledgerv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/ledger/v1"
)

// TopupPosting is one confirmed payment into VASPRO's paybill credited
// to a client's main wallet (task 4.4): Dr provider clearing on the
// rail, Cr main wallet.
type TopupPosting struct {
	TenantID uuid.UUID
	// NoticeID is the collection notice the money arrived on: the
	// posting's reference, and its idempotency key topup:<notice>, so a
	// repeat returns the same transaction.
	NoticeID    uuid.UUID
	Rail        string
	Currency    string
	AmountMinor int64
	Description string
}

// IdempotencyKey is the ledger key for p.
func (p TopupPosting) IdempotencyKey() string { return "topup:" + p.NoticeID.String() }

// Ledger posts top-ups to the ledger.
type Ledger interface {
	PostTopup(ctx context.Context, p TopupPosting) (transactionID uuid.UUID, err error)
}

// GRPCLedger is Ledger over the ledger's gRPC posting API.
type GRPCLedger struct {
	client ledgerv1.LedgerServiceClient
	conn   *gogrpc.ClientConn
}

// DialLedger connects lazily to the ledger's gRPC API at addr, so
// payments starts while the ledger is down; postings then wait for the
// sweep.
func DialLedger(addr string) (*GRPCLedger, error) {
	conn, err := gogrpc.NewClient(addr, gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("adapters: dialing the ledger at %s: %w", addr, err)
	}
	return NewGRPCLedger(conn), nil
}

// NewGRPCLedger uses an existing connection.
func NewGRPCLedger(conn *gogrpc.ClientConn) *GRPCLedger {
	return &GRPCLedger{client: ledgerv1.NewLedgerServiceClient(conn), conn: conn}
}

// PostTopup implements Ledger.
func (l *GRPCLedger) PostTopup(ctx context.Context, p TopupPosting) (uuid.UUID, error) {
	resp, err := l.client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{
		TenantId: p.TenantID.String(),
		Posting: &ledgerv1.Posting{
			IdempotencyKey: p.IdempotencyKey(),
			Type:           "topup",
			Description:    p.Description,
			ReferenceType:  "collection_notice",
			ReferenceId:    p.NoticeID.String(),
			Lines: []*ledgerv1.Line{
				{Account: &ledgerv1.AccountRef{Type: "provider_clearing", Rail: p.Rail, Currency: p.Currency}, Direction: "D", AmountMinor: p.AmountMinor},
				{Account: &ledgerv1.AccountRef{Type: "main_wallet", Currency: p.Currency}, Direction: "C", AmountMinor: p.AmountMinor},
			},
		},
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("adapters: posting the top-up: %w", err)
	}
	id, err := uuid.Parse(resp.GetTransactionId())
	if err != nil {
		return uuid.Nil, fmt.Errorf("adapters: the ledger returned a malformed transaction ID: %w", err)
	}
	return id, nil
}

// Close closes the connection.
func (l *GRPCLedger) Close() error { return l.conn.Close() }
