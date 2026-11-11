// Package grpc serves identity's internal gRPC API
// (proto/identity/v1) to other VASPRO services. Never exposed to
// clients.
package grpc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	identityv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/identity/v1"
)

// UseCases are the use cases this API exposes: app.Service's
// ResolveSession (the same check identity's own requireSession uses),
// and, for accounts' invitations and member lists (task 2.3b),
// EnsureInvitee, IsStepUpFresh and GetUsers.
type UseCases interface {
	ResolveSession(ctx context.Context, token string) (sessionID, userID uuid.UUID, err error)
	EnsureInvitee(ctx context.Context, email string) (uuid.UUID, error)
	SendInvitation(ctx context.Context, inviteeID, inviterID uuid.UUID, accountName string) error
	IsStepUpFresh(ctx context.Context, userID, sessionID uuid.UUID) (bool, error)
	GetUsers(ctx context.Context, userIDs []uuid.UUID) ([]app.UserSummary, error)
	StaffRole(ctx context.Context, userID uuid.UUID) (app.StaffRole, error)
}

// Server implements identityv1.IdentityServiceServer.
type Server struct {
	identityv1.UnimplementedIdentityServiceServer
	sessions UseCases
}

// Register adds identity's gRPC service to s.
func Register(s *gogrpc.Server, sessions UseCases) {
	identityv1.RegisterIdentityServiceServer(s, &Server{sessions: sessions})
}

// ResolveSession implements identityv1.IdentityServiceServer. Every
// rejected token is UNAUTHENTICATED alike; anything else that fails is
// INTERNAL with no detail, so nothing about the lookup leaks.
func (s *Server) ResolveSession(ctx context.Context, req *identityv1.ResolveSessionRequest) (*identityv1.ResolveSessionResponse, error) {
	token := req.GetSessionToken()
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "invalid session")
	}
	sessionID, userID, err := s.sessions.ResolveSession(ctx, token)
	if errors.Is(err, app.ErrNotFound) {
		return nil, status.Error(codes.Unauthenticated, "invalid session")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "resolving session failed")
	}
	return &identityv1.ResolveSessionResponse{SessionId: sessionID.String(), UserId: userID.String()}, nil
}

// EnsureInvitee implements identityv1.IdentityServiceServer.
func (s *Server) EnsureInvitee(ctx context.Context, req *identityv1.EnsureInviteeRequest) (*identityv1.EnsureInviteeResponse, error) {
	userID, err := s.sessions.EnsureInvitee(ctx, req.GetEmail())
	if errors.Is(err, app.ErrValidation) {
		return nil, status.Error(codes.InvalidArgument, "email is not a valid address")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "finding the invitee failed")
	}
	return &identityv1.EnsureInviteeResponse{UserId: userID.String()}, nil
}

// SendInvitation implements identityv1.IdentityServiceServer.
func (s *Server) SendInvitation(ctx context.Context, req *identityv1.SendInvitationRequest) (*identityv1.SendInvitationResponse, error) {
	invitee, err := uuid.Parse(req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "user_id must be a UUID")
	}
	inviter, err := uuid.Parse(req.GetInvitedByUserId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invited_by_user_id must be a UUID")
	}
	err = s.sessions.SendInvitation(ctx, invitee, inviter, req.GetAccountName())
	if errors.Is(err, app.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "unknown user")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "sending the invitation failed")
	}
	return &identityv1.SendInvitationResponse{}, nil
}

// IsStepUpFresh implements identityv1.IdentityServiceServer.
func (s *Server) IsStepUpFresh(ctx context.Context, req *identityv1.IsStepUpFreshRequest) (*identityv1.IsStepUpFreshResponse, error) {
	sessionID, err := uuid.Parse(req.GetSessionId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "session_id must be a UUID")
	}
	userID, err := uuid.Parse(req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "user_id must be a UUID")
	}
	fresh, err := s.sessions.IsStepUpFresh(ctx, userID, sessionID)
	if err != nil {
		return nil, status.Error(codes.Internal, "checking step-up failed")
	}
	return &identityv1.IsStepUpFreshResponse{Fresh: fresh}, nil
}

// IsStaff implements identityv1.IdentityServiceServer (task 2.9; the
// finance-lead flag since 3.7).
func (s *Server) IsStaff(ctx context.Context, req *identityv1.IsStaffRequest) (*identityv1.IsStaffResponse, error) {
	userID, err := uuid.Parse(req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "user_id must be a UUID")
	}
	role, err := s.sessions.StaffRole(ctx, userID)
	if err != nil {
		return nil, status.Error(codes.Internal, "checking staff failed")
	}
	return &identityv1.IsStaffResponse{IsStaff: role.Staff, FinanceLead: role.FinanceLead}, nil
}

// GetUsers implements identityv1.IdentityServiceServer.
func (s *Server) GetUsers(ctx context.Context, req *identityv1.GetUsersRequest) (*identityv1.GetUsersResponse, error) {
	ids := make([]uuid.UUID, len(req.GetUserIds()))
	for i, raw := range req.GetUserIds() {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "user_ids must be UUIDs")
		}
		ids[i] = id
	}
	users, err := s.sessions.GetUsers(ctx, ids)
	if err != nil {
		return nil, status.Error(codes.Internal, "listing users failed")
	}
	resp := &identityv1.GetUsersResponse{Users: make([]*identityv1.User, len(users))}
	for i, u := range users {
		resp.Users[i] = &identityv1.User{UserId: u.ID.String(), Email: u.Email, FullName: u.FullName}
	}
	return resp, nil
}
