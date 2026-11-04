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
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	accountsv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/accounts/v1"
)

// AccessChecker is the use case this API exposes:
// app.Service.CheckAccess, the same rule accounts' own routes apply.
type AccessChecker interface {
	CheckAccess(ctx context.Context, userID, tenantID uuid.UUID, permission string) error
}

// Server implements accountsv1.AccountsServiceServer.
type Server struct {
	accountsv1.UnimplementedAccountsServiceServer
	access AccessChecker
}

// Register adds accounts' gRPC service to s.
func Register(s *gogrpc.Server, access AccessChecker) {
	accountsv1.RegisterAccountsServiceServer(s, &Server{access: access})
}

// CheckTenantAccess implements accountsv1.AccountsServiceServer: OK when
// the user may work in the account (and their role allows the
// permission, if one is asked); no access and no such account are
// NOT_FOUND alike; a role without the permission is PERMISSION_DENIED;
// a permission outside the catalogue is INVALID_ARGUMENT; anything else
// is INTERNAL with no detail.
func (s *Server) CheckTenantAccess(ctx context.Context, req *accountsv1.CheckTenantAccessRequest) (*accountsv1.CheckTenantAccessResponse, error) {
	userID, err := uuid.Parse(req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "user_id must be a UUID")
	}
	tenantID, err := uuid.Parse(req.GetTenantId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "tenant_id must be a UUID")
	}
	switch err := s.access.CheckAccess(ctx, userID, tenantID, req.GetPermission()); {
	case err == nil:
	case errors.Is(err, app.ErrNotFound):
		return nil, status.Error(codes.NotFound, "no access to this account")
	case errors.Is(err, domain.ErrPermissionDenied):
		return nil, status.Error(codes.PermissionDenied, "the role doesn't allow this")
	case errors.Is(err, app.ErrUnknownPermission):
		return nil, status.Error(codes.InvalidArgument, "unknown permission")
	default:
		return nil, status.Error(codes.Internal, "checking access failed")
	}
	return &accountsv1.CheckTenantAccessResponse{}, nil
}
