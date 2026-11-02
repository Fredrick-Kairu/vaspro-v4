//go:build integration

package gateway

import (
	"context"
	"net"
	"testing"

	gogrpc "google.golang.org/grpc"

	accountsv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/accounts/v1"
)

// allowAllAccounts answers identity's CheckTenantAccess with OK for
// every user and account, for gateway tests that don't exercise
// accounts (task 2.2c): their sessions select any account ID.
type allowAllAccounts struct {
	accountsv1.UnimplementedAccountsServiceServer
}

func (allowAllAccounts) CheckTenantAccess(context.Context, *accountsv1.CheckTenantAccessRequest) (*accountsv1.CheckTenantAccessResponse, error) {
	return &accountsv1.CheckTenantAccessResponse{}, nil
}

// startAllowAllAccountsGRPC serves allowAllAccounts on a free local
// port and returns its address.
func startAllowAllAccountsGRPC(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := gogrpc.NewServer()
	accountsv1.RegisterAccountsServiceServer(srv, allowAllAccounts{})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}
