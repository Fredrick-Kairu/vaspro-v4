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

// SessionResolver is the one use case this API exposes:
// app.Service.ResolveSession, the same check identity's own
// requireSession uses.
type SessionResolver interface {
	ResolveSession(ctx context.Context, token string) (sessionID, userID uuid.UUID, err error)
}

// Server implements identityv1.IdentityServiceServer.
type Server struct {
	identityv1.UnimplementedIdentityServiceServer
	sessions SessionResolver
}

// Register adds identity's gRPC service to s.
func Register(s *gogrpc.Server, sessions SessionResolver) {
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
