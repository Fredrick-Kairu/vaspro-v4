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

// Identity finds staff members by email (task 4.3, OQ-22): the ops CLI
// records which staff member set up VASPRO's paybill and its
// credentials.
type Identity interface {
	StaffByEmail(ctx context.Context, email string) (uuid.UUID, error)
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

// Close closes the connection.
func (i *GRPCIdentity) Close() error { return i.conn.Close() }
