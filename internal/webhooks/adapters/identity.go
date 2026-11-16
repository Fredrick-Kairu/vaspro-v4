package adapters

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	identityv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/identity/v1"
)

// Identity answers whether a session's step-up is fresh: creating an
// endpoint, changing its URL and rotating its secret need one (system
// design §3, OQ-23).
type Identity interface {
	IsStepUpFresh(ctx context.Context, userID, sessionID uuid.UUID) (bool, error)
}

// GRPCIdentity is Identity over identity's gRPC API.
type GRPCIdentity struct {
	client identityv1.IdentityServiceClient
	conn   *gogrpc.ClientConn
}

// DialIdentity connects lazily to identity's gRPC API at addr.
func DialIdentity(addr string) (*GRPCIdentity, error) {
	conn, err := gogrpc.NewClient(addr, gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("adapters: dialing identity at %s: %w", addr, err)
	}
	return &GRPCIdentity{client: identityv1.NewIdentityServiceClient(conn), conn: conn}, nil
}

// IsStepUpFresh is never true for another user's session.
func (i *GRPCIdentity) IsStepUpFresh(ctx context.Context, userID, sessionID uuid.UUID) (bool, error) {
	resp, err := i.client.IsStepUpFresh(ctx, &identityv1.IsStepUpFreshRequest{SessionId: sessionID.String(), UserId: userID.String()})
	if err != nil {
		return false, fmt.Errorf("adapters: checking step-up: %w", err)
	}
	return resp.GetFresh(), nil
}

// Close closes the connection.
func (i *GRPCIdentity) Close() error { return i.conn.Close() }
