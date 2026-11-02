package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/accounts/v1"
)

// ErrNoTenantAccess means the user may not work in the account: not a
// member (or no longer), or no such account, alike.
var ErrNoTenantAccess = errors.New("adapters: no access to this account")

// TenantAccess asks accounts whether a user may work in an account
// (task 2.2c, decision Q2: identity asks accounts over gRPC, so a
// removed member loses access at once).
type TenantAccess interface {
	CheckTenantAccess(ctx context.Context, userID, tenantID uuid.UUID) error
}

// GRPCTenantAccess is TenantAccess over accounts' gRPC API.
type GRPCTenantAccess struct {
	client accountsv1.AccountsServiceClient
	conn   *gogrpc.ClientConn
}

// DialTenantAccess connects lazily to accounts' gRPC API at addr
// (plaintext on the internal network; mTLS is Phase 17), so identity
// starts even while accounts is down.
func DialTenantAccess(addr string) (*GRPCTenantAccess, error) {
	conn, err := gogrpc.NewClient(addr, gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("adapters: dialing accounts at %s: %w", addr, err)
	}
	return &GRPCTenantAccess{client: accountsv1.NewAccountsServiceClient(conn), conn: conn}, nil
}

// CheckTenantAccess returns ErrNoTenantAccess for NOT_FOUND and wraps
// every other failure, so a caller fails closed either way.
func (a *GRPCTenantAccess) CheckTenantAccess(ctx context.Context, userID, tenantID uuid.UUID) error {
	_, err := a.client.CheckTenantAccess(ctx, &accountsv1.CheckTenantAccessRequest{UserId: userID.String(), TenantId: tenantID.String()})
	if status.Code(err) == codes.NotFound {
		return ErrNoTenantAccess
	}
	if err != nil {
		return fmt.Errorf("adapters: checking account access: %w", err)
	}
	return nil
}

// Close closes the connection.
func (a *GRPCTenantAccess) Close() error { return a.conn.Close() }
