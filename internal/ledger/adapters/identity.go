// Package adapters holds the ledger's outbound integrations behind
// interfaces (.claude/rules/go.md); fakes live in tools/fakes.
package adapters

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	identityv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/identity/v1"
)

// Identity is what the ledger asks identity before a staff action (task
// 3.4b): whether a user is staff, asked on every call so a revocation
// takes effect at once (2.9), and whether a session's step-up is fresh,
// before a price is approved or withdrawn.
type Identity interface {
	IsStaff(ctx context.Context, userID uuid.UUID) (bool, error)
	IsStepUpFresh(ctx context.Context, userID, sessionID uuid.UUID) (bool, error)
}

// GRPCIdentity is Identity over identity's gRPC API.
type GRPCIdentity struct {
	client identityv1.IdentityServiceClient
	conn   *gogrpc.ClientConn
}

// DialIdentity connects lazily to identity's gRPC API at addr
// (plaintext on the internal network; mTLS is Phase 17), so the ledger
// starts even while identity is down.
func DialIdentity(addr string) (*GRPCIdentity, error) {
	conn, err := gogrpc.NewClient(addr, gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("adapters: dialing identity at %s: %w", addr, err)
	}
	return &GRPCIdentity{client: identityv1.NewIdentityServiceClient(conn), conn: conn}, nil
}

func (g *GRPCIdentity) IsStaff(ctx context.Context, userID uuid.UUID) (bool, error) {
	resp, err := g.client.IsStaff(ctx, &identityv1.IsStaffRequest{UserId: userID.String()})
	if err != nil {
		return false, fmt.Errorf("adapters: checking staff: %w", err)
	}
	return resp.GetIsStaff(), nil
}

func (g *GRPCIdentity) IsStepUpFresh(ctx context.Context, userID, sessionID uuid.UUID) (bool, error) {
	resp, err := g.client.IsStepUpFresh(ctx, &identityv1.IsStepUpFreshRequest{SessionId: sessionID.String(), UserId: userID.String()})
	if err != nil {
		return false, fmt.Errorf("adapters: checking step-up: %w", err)
	}
	return resp.GetFresh(), nil
}

// Close closes the connection.
func (g *GRPCIdentity) Close() error { return g.conn.Close() }
