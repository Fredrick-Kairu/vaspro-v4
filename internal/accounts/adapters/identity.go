// Package adapters holds accounts' outbound integrations behind
// interfaces (.claude/rules/go.md); fakes live in tools/fakes.
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

// ErrInvalidEmail means identity refused an invitation address.
var ErrInvalidEmail = errors.New("adapters: not a valid email address")

// User is what identity shares about a user for member lists.
type User struct {
	ID       uuid.UUID
	Email    string
	FullName string
}

// Identity is what accounts asks identity (task 2.3b): who an
// invitation is for, the invitation email, whether a session's step-up
// is fresh, and members' names and emails.
type Identity interface {
	EnsureInvitee(ctx context.Context, email string) (uuid.UUID, error)
	SendInvitation(ctx context.Context, inviteeID, inviterID uuid.UUID, accountName string) error
	IsStepUpFresh(ctx context.Context, userID, sessionID uuid.UUID) (bool, error)
	GetUsers(ctx context.Context, userIDs []uuid.UUID) ([]User, error)
}

// GRPCIdentity is Identity over identity's gRPC API.
type GRPCIdentity struct {
	client identityv1.IdentityServiceClient
	conn   *gogrpc.ClientConn
}

// DialIdentity connects lazily to identity's gRPC API at addr
// (plaintext on the internal network; mTLS is Phase 17), so accounts
// starts even while identity is down.
func DialIdentity(addr string) (*GRPCIdentity, error) {
	conn, err := gogrpc.NewClient(addr, gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("adapters: dialing identity at %s: %w", addr, err)
	}
	return &GRPCIdentity{client: identityv1.NewIdentityServiceClient(conn), conn: conn}, nil
}

func (g *GRPCIdentity) EnsureInvitee(ctx context.Context, email string) (uuid.UUID, error) {
	resp, err := g.client.EnsureInvitee(ctx, &identityv1.EnsureInviteeRequest{Email: email})
	if status.Code(err) == codes.InvalidArgument {
		return uuid.Nil, ErrInvalidEmail
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("adapters: finding the invitee: %w", err)
	}
	id, err := uuid.Parse(resp.GetUserId())
	if err != nil {
		return uuid.Nil, fmt.Errorf("adapters: identity returned a bad user id: %w", err)
	}
	return id, nil
}

func (g *GRPCIdentity) SendInvitation(ctx context.Context, inviteeID, inviterID uuid.UUID, accountName string) error {
	if _, err := g.client.SendInvitation(ctx, &identityv1.SendInvitationRequest{
		UserId: inviteeID.String(), InvitedByUserId: inviterID.String(), AccountName: accountName,
	}); err != nil {
		return fmt.Errorf("adapters: sending the invitation: %w", err)
	}
	return nil
}

func (g *GRPCIdentity) IsStepUpFresh(ctx context.Context, userID, sessionID uuid.UUID) (bool, error) {
	resp, err := g.client.IsStepUpFresh(ctx, &identityv1.IsStepUpFreshRequest{SessionId: sessionID.String(), UserId: userID.String()})
	if err != nil {
		return false, fmt.Errorf("adapters: checking step-up: %w", err)
	}
	return resp.GetFresh(), nil
}

func (g *GRPCIdentity) GetUsers(ctx context.Context, userIDs []uuid.UUID) ([]User, error) {
	req := &identityv1.GetUsersRequest{UserIds: make([]string, len(userIDs))}
	for i, id := range userIDs {
		req.UserIds[i] = id.String()
	}
	resp, err := g.client.GetUsers(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("adapters: listing users: %w", err)
	}
	users := make([]User, 0, len(resp.GetUsers()))
	for _, u := range resp.GetUsers() {
		id, err := uuid.Parse(u.GetUserId())
		if err != nil {
			continue
		}
		users = append(users, User{ID: id, Email: u.GetEmail(), FullName: u.GetFullName()})
	}
	return users, nil
}

// Close closes the connection.
func (g *GRPCIdentity) Close() error { return g.conn.Close() }
