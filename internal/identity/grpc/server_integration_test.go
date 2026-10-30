//go:build integration

package grpc_test

import (
	"context"
	"net"
	"testing"
	"time"

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
