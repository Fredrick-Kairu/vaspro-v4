// Package grpc serves accounts' internal gRPC API (proto/accounts/v1)
// to other VASPRO services. Never exposed to clients.
package grpc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	accountsv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/accounts/v1"
)

// TenantReader is the use case this API exposes: app.Service.GetTenant,
// the same access rule accounts' own getTenant applies.
type TenantReader interface {
	GetTenant(ctx context.Context, userID, tenantID uuid.UUID) (*app.Tenant, error)
}

// Server implements accountsv1.AccountsServiceServer.
type Server struct {
	accountsv1.UnimplementedAccountsServiceServer
	tenants TenantReader
}

// Register adds accounts' gRPC service to s.
func Register(s *gogrpc.Server, tenants TenantReader) {
	accountsv1.RegisterAccountsServiceServer(s, &Server{tenants: tenants})
}

// CheckTenantAccess implements accountsv1.AccountsServiceServer: OK when
// the user may work in the account; no access and no such account are
// NOT_FOUND alike; anything else is INTERNAL with no detail.
func (s *Server) CheckTenantAccess(ctx context.Context, req *accountsv1.CheckTenantAccessRequest) (*accountsv1.CheckTenantAccessResponse, error) {
	userID, err := uuid.Parse(req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "user_id must be a UUID")
	}
	tenantID, err := uuid.Parse(req.GetTenantId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "tenant_id must be a UUID")
	}
	if _, err := s.tenants.GetTenant(ctx, userID, tenantID); err != nil {
		if errors.Is(err, app.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "no access to this account")
		}
		return nil, status.Error(codes.Internal, "checking access failed")
	}
	return &accountsv1.CheckTenantAccessResponse{}, nil
}
