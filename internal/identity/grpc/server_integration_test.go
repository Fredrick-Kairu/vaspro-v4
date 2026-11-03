//go:build integration

package grpc_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	identitygrpc "github.com/Fredrick-Kairu/vaspro-v4/internal/identity/grpc"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	identityv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/identity/v1"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// newClient serves identity's gRPC API over an in-memory listener,
// backed by a real app.Service on real Postgres and Redis.
func newClient(t *testing.T) (identityv1.IdentityServiceClient, *app.Service, *email.Sender) {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/identity"))
	pool, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=identity,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var c *cache.Cache = testkit.StartRedis(t)
	sender := &email.Sender{}
	svc := app.New(app.Deps{Pool: pool, Email: sender, Clock: testkit.NewClock(time.Now().UTC()), Cache: c})

	lis := bufconn.Listen(1 << 20)
	srv := gogrpc.NewServer()
	identitygrpc.Register(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := gogrpc.NewClient("passthrough:///bufnet",
		gogrpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return identityv1.NewIdentityServiceClient(conn), svc, sender
}

// signIn creates a verified user with a password and returns a live
// session.
func signIn(t *testing.T, svc *app.Service, sender *email.Sender) *app.Session {
	t.Helper()
	ctx := context.Background()
	const addr, password = "ada@example.com", "a-strong-password"
	if err := svc.SignUp(ctx, addr, "Ada Lovelace", nil); err != nil {
		t.Fatal(err)
	}
	sent, _ := sender.LastOfKind(email.KindVerification)
	if err := svc.VerifyEmail(ctx, addr, sent.Code); err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestPasswordReset(ctx, addr); err != nil {
		t.Fatal(err)
	}
	sent, _ = sender.LastOfKind(email.KindPasswordReset)
	if err := svc.ConfirmPasswordReset(ctx, addr, sent.Code, password); err != nil {
		t.Fatal(err)
	}
	result, err := svc.SignIn(ctx, addr, password, nil, "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	return result.Session
}

func TestResolveSession(t *testing.T) {
	client, svc, sender := newClient(t)
	ctx := context.Background()
	session := signIn(t, svc, sender)

	t.Run("a live session token resolves to its session and user", func(t *testing.T) {
		resp, err := client.ResolveSession(ctx, &identityv1.ResolveSessionRequest{SessionToken: session.Token})
		if err != nil {
			t.Fatalf("ResolveSession: %v", err)
		}
		if resp.GetSessionId() != session.ID.String() || resp.GetUserId() == "" {
			t.Fatalf("got %+v, want session %s and a user", resp, session.ID)
		}
	})
	for name, token := range map[string]string{
		"an empty token":              "",
		"a garbage token":             "sess_not-a-real-token",
		"the session ID, not a token": session.ID.String(),
	} {
		t.Run(name+" is UNAUTHENTICATED", func(t *testing.T) {
			_, err := client.ResolveSession(ctx, &identityv1.ResolveSessionRequest{SessionToken: token})
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("got %v, want Unauthenticated", err)
			}
		})
	}
	t.Run("a revoked session is UNAUTHENTICATED", func(t *testing.T) {
		_, userID, err := svc.ResolveSession(ctx, session.Token)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SignOutEverywhere(ctx, userID); err != nil {
			t.Fatal(err)
		}
		_, err = client.ResolveSession(ctx, &identityv1.ResolveSessionRequest{SessionToken: session.Token})
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("got %v, want Unauthenticated", err)
		}
	})
}

// signInAs is signIn for any address.
func signInAs(t *testing.T, svc *app.Service, sender *email.Sender, addr string) (session *app.Session, userID string) {
	t.Helper()
	ctx := context.Background()
	const password = "a-strong-password"
	if err := svc.SignUp(ctx, addr, "Test User", nil); err != nil {
		t.Fatal(err)
	}
	sent, _ := sender.LastOfKind(email.KindVerification)
	if err := svc.VerifyEmail(ctx, addr, sent.Code); err != nil {
		t.Fatal(err)
	}
	if err := svc.RequestPasswordReset(ctx, addr); err != nil {
		t.Fatal(err)
	}
	sent, _ = sender.LastOfKind(email.KindPasswordReset)
	if err := svc.ConfirmPasswordReset(ctx, addr, sent.Code, password); err != nil {
		t.Fatal(err)
	}
	result, err := svc.SignIn(ctx, addr, password, nil, "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	_, uid, err := svc.ResolveSession(ctx, result.Session.Token)
	if err != nil {
		t.Fatal(err)
	}
	return result.Session, uid.String()
}

func TestIsStepUpFresh(t *testing.T) {
	client, svc, sender := newClient(t)
	ctx := context.Background()
	session, user := signInAs(t, svc, sender, "ada@example.com")
	other, otherUser := signInAs(t, svc, sender, "bob@example.com")

	fresh := func(sessionID, userID string) bool {
		t.Helper()
		resp, err := client.IsStepUpFresh(ctx, &identityv1.IsStepUpFreshRequest{SessionId: sessionID, UserId: userID})
		if err != nil {
			t.Fatalf("IsStepUpFresh: %v", err)
		}
		return resp.GetFresh()
	}
	if fresh(session.ID.String(), user) {
		t.Fatal("fresh before any step-up")
	}
	if err := svc.RequestStepUpEmail(ctx, uuid.MustParse(user)); err != nil {
		t.Fatal(err)
	}
	sent, _ := sender.LastOfKind(email.KindStepUp)
	if err := svc.ConfirmStepUpEmail(ctx, uuid.MustParse(user), session.ID, sent.Code); err != nil {
		t.Fatal(err)
	}
	if !fresh(session.ID.String(), user) {
		t.Fatal("not fresh right after stepping up")
	}
	if fresh(session.ID.String(), otherUser) {
		t.Fatal("ada's session is fresh when asked as bob's")
	}
	if fresh(other.ID.String(), otherUser) {
		t.Fatal("bob's session is fresh without stepping up")
	}
	if _, err := client.IsStepUpFresh(ctx, &identityv1.IsStepUpFreshRequest{SessionId: "x", UserId: user}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a malformed session id: got %v, want InvalidArgument", err)
	}
}

func TestEnsureInviteeAndGetUsers(t *testing.T) {
	client, svc, sender := newClient(t)
	ctx := context.Background()
	_, ada := signInAs(t, svc, sender, "ada@example.com")

	resp, err := client.EnsureInvitee(ctx, &identityv1.EnsureInviteeRequest{Email: "carol@example.com"})
	if err != nil || resp.GetUserId() == "" {
		t.Fatalf("EnsureInvitee: %+v, %v", resp, err)
	}
	if _, ok := sender.LastOfKind(email.KindInvitation); ok {
		t.Fatal("EnsureInvitee sent an email; only SendInvitation should")
	}
	if _, err := client.SendInvitation(ctx, &identityv1.SendInvitationRequest{UserId: resp.GetUserId(), InvitedByUserId: ada, AccountName: "Acme"}); err != nil {
		t.Fatalf("SendInvitation: %v", err)
	}
	if sent, _ := sender.LastOfKind(email.KindInvitation); sent.To != "carol@example.com" || sent.AccountName != "Acme" {
		t.Fatalf("got email %+v", sent)
	}
	if _, err := client.SendInvitation(ctx, &identityv1.SendInvitationRequest{UserId: uuid.NewString(), InvitedByUserId: ada, AccountName: "Acme"}); status.Code(err) != codes.NotFound {
		t.Fatalf("an unknown invitee: got %v, want NotFound", err)
	}
	if _, err := client.EnsureInvitee(ctx, &identityv1.EnsureInviteeRequest{Email: "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a malformed email: got %v, want InvalidArgument", err)
	}

	users, err := client.GetUsers(ctx, &identityv1.GetUsersRequest{UserIds: []string{ada, resp.GetUserId(), uuid.NewString()}})
	if err != nil || len(users.GetUsers()) != 2 {
		t.Fatalf("GetUsers: %+v, %v", users, err)
	}
	if _, err := client.GetUsers(ctx, &identityv1.GetUsersRequest{UserIds: []string{"x"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a malformed id: got %v, want InvalidArgument", err)
	}
}

func TestIsStaff(t *testing.T) {
	client, svc, sender := newClient(t)
	ctx := context.Background()
	_, ada := signInAs(t, svc, sender, "ada@example.com")
	_, bob := signInAs(t, svc, sender, "bob@example.com")
	if err := svc.GrantStaff(ctx, "ada@example.com", "ops"); err != nil {
		t.Fatal(err)
	}
	isStaff := func(userID string) bool {
		t.Helper()
		resp, err := client.IsStaff(ctx, &identityv1.IsStaffRequest{UserId: userID})
		if err != nil {
			t.Fatalf("IsStaff: %v", err)
		}
		return resp.GetIsStaff()
	}
	if !isStaff(ada) {
		t.Fatal("ada was granted staff")
	}
	if isStaff(bob) || isStaff(uuid.NewString()) {
		t.Fatal("a stranger or unknown user is staff")
	}
	if err := svc.RevokeStaff(ctx, "ada@example.com", "ops"); err != nil {
		t.Fatal(err)
	}
	if isStaff(ada) {
		t.Fatal("revoked staff still staff")
	}
	if _, err := client.IsStaff(ctx, &identityv1.IsStaffRequest{UserId: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("malformed id: got %v, want InvalidArgument", err)
	}
}
