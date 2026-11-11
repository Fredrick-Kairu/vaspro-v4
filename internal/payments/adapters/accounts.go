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

// ErrPermissionDenied means the user is in the account but their role
// doesn't allow the permission asked for.
var ErrPermissionDenied = errors.New("adapters: the role doesn't allow this")

// TenantInfo is what accounts tells payments about an account it may
// work in (task 4.3, OQ-22).
type TenantInfo struct {
	CountryCode string
	Currency    string
}

// Accounts asks accounts whether a dashboard user may work in an
// account with a permission (billing:read to see top-up details), and
// learns the account's country and currency. Asked on every call, so a
// removed member loses access at once.
type Accounts interface {
	TenantAccess(ctx context.Context, userID, tenantID uuid.UUID, permission string) (TenantInfo, error)
}

// GRPCAccounts is Accounts over accounts' gRPC API.
type GRPCAccounts struct {
	client accountsv1.AccountsServiceClient
	conn   *gogrpc.ClientConn
}

// DialAccounts connects lazily to accounts' gRPC API at addr
// (plaintext on the internal network; mTLS is Phase 17), so payments
// starts while accounts is down; dashboard calls then fail closed.
func DialAccounts(addr string) (*GRPCAccounts, error) {
	conn, err := gogrpc.NewClient(addr, gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("adapters: dialing accounts at %s: %w", addr, err)
	}
	return &GRPCAccounts{client: accountsv1.NewAccountsServiceClient(conn), conn: conn}, nil
}

// TenantAccess returns ErrNoTenantAccess for NOT_FOUND,
// ErrPermissionDenied for PERMISSION_DENIED, and wraps every other
// failure, so a caller fails closed either way.
func (a *GRPCAccounts) TenantAccess(ctx context.Context, userID, tenantID uuid.UUID, permission string) (TenantInfo, error) {
	resp, err := a.client.CheckTenantAccess(ctx, &accountsv1.CheckTenantAccessRequest{
		UserId: userID.String(), TenantId: tenantID.String(), Permission: permission,
	})
	switch status.Code(err) {
	case codes.NotFound:
		return TenantInfo{}, ErrNoTenantAccess
	case codes.PermissionDenied:
		return TenantInfo{}, ErrPermissionDenied
	}
	if err != nil {
		return TenantInfo{}, fmt.Errorf("adapters: checking account access: %w", err)
	}
	return TenantInfo{CountryCode: resp.GetCountryCode(), Currency: resp.GetCurrency()}, nil
}

// Close closes the connection.
func (a *GRPCAccounts) Close() error { return a.conn.Close() }
