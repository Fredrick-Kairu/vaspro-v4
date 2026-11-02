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

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	accountsgrpc "github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/grpc"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	accountsv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/accounts/v1"
)

// newClient serves accounts' gRPC API over an in-memory listener,
// backed by a real app.Service on real Postgres.
func newClient(t *testing.T) (accountsv1.AccountsServiceClient, *app.Service, *vasdb.Pool) {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/accounts"))
	pool, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=accounts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	svc := app.New(app.Deps{Pool: pool, Clock: testkit.NewClock(time.Now().UTC()), Packs: map[string]*packs.Pack{"KE": ke}})

	lis := bufconn.Listen(1 << 20)
	srv := gogrpc.NewServer()
	accountsgrpc.Register(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := gogrpc.NewClient("passthrough:///bufnet",
		gogrpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return accountsv1.NewAccountsServiceClient(conn), svc, pool
}

func TestCheckTenantAccess(t *testing.T) {
	client, svc, pool := newClient(t)
	ctx := context.Background()

	owner, removed, stranger, dev, viewer := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tenant, err := svc.CreateTenant(ctx, owner, domain.NewTenantInput{DisplayName: "Agency", CountryCode: "KE"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role_id, status) VALUES ($1, $2, $3, 'removed')`,
		tenant.ID, removed, domain.BuiltinAdminRoleID); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[uuid.UUID]uuid.UUID{dev: domain.BuiltinDeveloperRoleID, viewer: domain.BuiltinViewerRoleID} {
		if _, err := pool.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role_id, status) VALUES ($1, $2, $3, 'active')`,
			tenant.ID, user, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.ChangeTier(ctx, app.ChangeTierRequest{TenantID: tenant.ID, To: domain.TierFinancial, Reason: "contract", ChangedBy: ptr(uuid.New())}); err != nil {
		t.Fatal(err)
	}
	if err := svc.MakeReseller(ctx, app.MakeResellerRequest{TenantID: tenant.ID, ChangedBy: uuid.New(), Reason: "agreement"}); err != nil {
		t.Fatal(err)
	}
	sub, err := svc.CreateSubAccount(ctx, owner, tenant.ID, app.SubAccountInput{DisplayName: "Client"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name         string
		user, tenant string
		permission   string
		want         codes.Code
	}{
		{"a Developer may manage credentials", dev.String(), tenant.ID.String(), "credentials:manage", codes.OK},
		{"a Viewer may read credentials", viewer.String(), tenant.ID.String(), "credentials:read", codes.OK},
		{"a Viewer may not manage credentials", viewer.String(), tenant.ID.String(), "credentials:manage", codes.PermissionDenied},
		{"a Viewer of the reseller may not manage the sub-account's", viewer.String(), sub.ID.String(), "credentials:manage", codes.PermissionDenied},
		{"a name outside the catalogue", owner.String(), tenant.ID.String(), "payouts:approve", codes.InvalidArgument},
		{"a stranger is still not found, whatever the permission", stranger.String(), tenant.ID.String(), "credentials:read", codes.NotFound},
		{"a member", owner.String(), tenant.ID.String(), "", codes.OK},
		{"a member of the reseller, for a sub-account", owner.String(), sub.ID.String(), "", codes.OK},
		{"a removed member", removed.String(), tenant.ID.String(), "", codes.NotFound},
		{"a stranger", stranger.String(), tenant.ID.String(), "", codes.NotFound},
		{"an unknown account", owner.String(), uuid.New().String(), "", codes.NotFound},
		{"a malformed user id", "not-a-uuid", tenant.ID.String(), "", codes.InvalidArgument},
		{"a malformed tenant id", owner.String(), "", "", codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.CheckTenantAccess(ctx, &accountsv1.CheckTenantAccessRequest{UserId: tc.user, TenantId: tc.tenant, Permission: tc.permission})
			if got := status.Code(err); got != tc.want {
				t.Fatalf("got %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
