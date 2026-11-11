package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ledgerv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/ledger/v1"
)

// TopupKind is how a payment's money moves (tasks 4.4, 4.5).
type TopupKind int

const (
	TopupDirect       TopupKind = iota // provider clearing → the client's main wallet
	TopupToSuspense                    // provider clearing → suspense, no client yet
	TopupFromSuspense                  // suspense → the client's main wallet, a staff match
)

// TopupPosting is one confirmed payment into VASPRO's paybill credited
// to a client's main wallet (task 4.4): Dr provider clearing on the
// rail, Cr main wallet.
type TopupPosting struct {
	Kind     TopupKind
	TenantID uuid.UUID // ignored for TopupToSuspense
	// NoticeID is the collection notice the money arrived on: the
	// posting's reference, and its idempotency key topup:<notice>, so a
	// repeat returns the same transaction.
	NoticeID    uuid.UUID
	Rail        string
	Currency    string
	AmountMinor int64
	Description string
}

// IdempotencyKey is the ledger key for p: one per notice and kind.
func (p TopupPosting) IdempotencyKey() string {
	switch p.Kind {
	case TopupToSuspense:
		return "suspense:" + p.NoticeID.String()
	case TopupFromSuspense:
		return "match:" + p.NoticeID.String()
	default:
		return "topup:" + p.NoticeID.String()
	}
}

// legs are the accounts p debits and credits, and the tenant it posts
// for ("" is a system posting).
func (p TopupPosting) legs() (tenant string, from, to *ledgerv1.AccountRef) {
	clearing := &ledgerv1.AccountRef{Type: "provider_clearing", Rail: p.Rail, Currency: p.Currency}
	suspense := &ledgerv1.AccountRef{Type: "suspense", Currency: p.Currency}
	main := &ledgerv1.AccountRef{Type: "main_wallet", Currency: p.Currency}
	switch p.Kind {
	case TopupToSuspense:
		return "", clearing, suspense
	case TopupFromSuspense:
		return p.TenantID.String(), suspense, main
	default:
		return p.TenantID.String(), clearing, main
	}
}

// ClearingLine is one line on a rail's provider clearing account (task
// 4.6): what reconciliation checks the day's payments against.
type ClearingLine struct {
	EntryID       int64
	TransactionID uuid.UUID
	Direction     string // D or C
	AmountMinor   int64
	ReferenceType string
	ReferenceID   uuid.UUID // uuid.Nil if none
	PostedAt      time.Time
}

// Ledger posts top-ups to the ledger and reads provider clearing back.
type Ledger interface {
	PostTopup(ctx context.Context, p TopupPosting) (transactionID uuid.UUID, err error)
	// ClearingEntries is every line on rail's provider clearing in
	// currency posted in [from, to).
	ClearingEntries(ctx context.Context, rail, currency string, from, to time.Time) ([]ClearingLine, error)
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
	tenant, from, to := p.legs()
	resp, err := l.client.PostTransaction(ctx, &ledgerv1.PostTransactionRequest{
		TenantId: tenant,
		Posting: &ledgerv1.Posting{
			IdempotencyKey: p.IdempotencyKey(),
			Type:           "topup",
			Description:    p.Description,
			ReferenceType:  "collection_notice",
			ReferenceId:    p.NoticeID.String(),
			Lines: []*ledgerv1.Line{
				{Account: from, Direction: "D", AmountMinor: p.AmountMinor},
				{Account: to, Direction: "C", AmountMinor: p.AmountMinor},
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

// ClearingEntries implements Ledger, reading every page.
func (l *GRPCLedger) ClearingEntries(ctx context.Context, rail, currency string, from, to time.Time) ([]ClearingLine, error) {
	var (
		out   []ClearingLine
		after int64
	)
	for {
		resp, err := l.client.ListClearingEntries(ctx, &ledgerv1.ListClearingEntriesRequest{
			Rail: rail, Currency: currency, PostedFromUnix: from.Unix(), PostedToUnix: to.Unix(), AfterEntryId: after,
		})
		if err != nil {
			return nil, fmt.Errorf("adapters: listing clearing entries: %w", err)
		}
		for _, e := range resp.GetEntries() {
			txn, err := uuid.Parse(e.GetTransactionId())
			if err != nil {
				return nil, fmt.Errorf("adapters: the ledger returned a malformed transaction ID: %w", err)
			}
			line := ClearingLine{
				EntryID: e.GetEntryId(), TransactionID: txn, Direction: e.GetDirection(), AmountMinor: e.GetAmountMinor(),
				ReferenceType: e.GetReferenceType(), PostedAt: time.Unix(e.GetPostedAtUnix(), 0).UTC(),
			}
			if ref := e.GetReferenceId(); ref != "" {
				if line.ReferenceID, err = uuid.Parse(ref); err != nil {
					return nil, fmt.Errorf("adapters: the ledger returned a malformed reference: %w", err)
				}
			}
			out = append(out, line)
		}
		if resp.GetNextAfterEntryId() == 0 {
			return out, nil
		}
		after = resp.GetNextAfterEntryId()
	}
}

// Close closes the connection.
func (l *GRPCLedger) Close() error { return l.conn.Close() }
