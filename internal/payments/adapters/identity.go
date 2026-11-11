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

	identityv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/identity/v1"
)

// ErrNotStaff means the email isn't a staff member's: not staff, or no
// such user, alike.
var ErrNotStaff = errors.New("adapters: not a staff member")

// Identity is what payments asks identity about staff: the ops CLI
// finds a staff member by email to record who set up VASPRO's paybill
// (task 4.3), and staff routes ask on every call whether the user is
// staff and, before matching money, whether their step-up is fresh
// (task 4.5).
type Identity interface {
	StaffByEmail(ctx context.Context, email string) (uuid.UUID, error)
	IsStaff(ctx context.Context, userID uuid.UUID) (bool, error)
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

// StaffByEmail returns ErrNotStaff for NOT_FOUND or INVALID_ARGUMENT
// and wraps every other failure, so a caller fails closed.
func (i *GRPCIdentity) StaffByEmail(ctx context.Context, email string) (uuid.UUID, error) {
	resp, err := i.client.GetStaffByEmail(ctx, &identityv1.GetStaffByEmailRequest{Email: email})
	switch status.Code(err) {
	case codes.NotFound, codes.InvalidArgument:
		return uuid.Nil, ErrNotStaff
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("adapters: looking up staff: %w", err)
	}
	id, err := uuid.Parse(resp.GetUserId())
	if err != nil {
		return uuid.Nil, fmt.Errorf("adapters: identity returned a malformed user ID: %w", err)
	}
	return id, nil
}

// IsStaff asks on every call, so a revocation takes effect at once.
func (i *GRPCIdentity) IsStaff(ctx context.Context, userID uuid.UUID) (bool, error) {
	resp, err := i.client.IsStaff(ctx, &identityv1.IsStaffRequest{UserId: userID.String()})
	if err != nil {
		return false, fmt.Errorf("adapters: checking staff: %w", err)
	}
	return resp.GetIsStaff(), nil
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
