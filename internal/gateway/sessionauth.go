package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
	identityv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/identity/v1"
)

// The headers the gateway sets on a request it forwards to a backend
// other than identity, once it has checked the dashboard session (task
// 2.2a; system design §3: "the gateway checks every call before any
// service sees it"). Backends trust them because only the gateway can
// reach them; any client-sent copy is removed first.
const (
	headerUserID    = "X-Vaspro-User-Id"
	headerSessionID = "X-Vaspro-Session-Id"
)

// errInvalidSession means the token is missing, unknown, expired or
// revoked.
var errInvalidSession = errors.New("gateway: invalid session")

// SessionResolver checks a dashboard session token. Production uses
// identity's ResolveSession RPC (GRPCSessions); tests use a fake.
type SessionResolver interface {
	ResolveSession(ctx context.Context, token string) (sessionID, userID string, err error)
}

// headerClientIP carries the caller's address (callerIP: the client a
// trusted proxy named, otherwise the connection's peer) (task 4.4):
// payments checks a rail callback's source against it. Duplicated from
// payments' api.HeaderClientIP.
const headerClientIP = "X-Vaspro-Client-Ip"

// stripForwardedIdentity removes any client-sent forwarded-identity
// headers from every request, whichever backend it goes to, and sets
// the client address the gateway itself saw.
func stripForwardedIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(headerUserID)
		r.Header.Del(headerSessionID)
		r.Header.Del(headerClientIP)
		if ip, ok := callerIP(r); ok {
			r.Header.Set(headerClientIP, ip.String())
		}
		next.ServeHTTP(w, r)
	})
}

// requireSessionFor checks the Authorization: Bearer session token with
// sessions and forwards the session's user in the trusted headers. The
// token itself is not forwarded: the backend never needs it. A missing
// or invalid token is AUTH-4010, before the backend is called.
func requireSessionFor(sessions SessionResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				problem.Respond(w, r, problem.New(problem.AUTH4010, "a bearer session token is required"))
				return
			}
			sessionID, userID, err := sessions.ResolveSession(r.Context(), token)
			if errors.Is(err, errInvalidSession) {
				problem.Respond(w, r, problem.New(problem.AUTH4010, "a bearer session token is required"))
				return
			}
			if err != nil {
				problem.Respond(w, r, problem.New(problem.SYS9000, "the session could not be checked"))
				return
			}
			r.Header.Del("Authorization")
			r.Header.Set(headerUserID, userID)
			r.Header.Set(headerSessionID, sessionID)
			next.ServeHTTP(w, r)
		})
	}
}

// requireSessionUnlessPublic is requireSessionFor except for an
// operation the backend's spec declares public (security: []), such as
// a KYC vendor's webhook (task 2.6): the spec, not a list here, says
// which. A request no spec route matches still needs a session. The
// client-sent identity headers are stripped for every request before
// this runs (stripForwardedIdentity), so a public route never sees a
// spoofed user.
func requireSessionUnlessPublic(sessions SessionResolver, routes routers.Router) func(http.Handler) http.Handler {
	requireSession := requireSessionFor(sessions)
	return func(next http.Handler) http.Handler {
		withSession := requireSession(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if route, _, err := routes.FindRoute(r); err == nil && isPublic(route.Operation) {
				next.ServeHTTP(w, r)
				return
			}
			withSession.ServeHTTP(w, r)
		})
	}
}

// isPublic reports whether op's spec says it needs no credentials: an
// explicit, empty security list.
func isPublic(op *openapi3.Operation) bool {
	return op != nil && op.Security != nil && len(*op.Security) == 0
}

// GRPCSessions resolves sessions through identity's ResolveSession RPC.
type GRPCSessions struct {
	client identityv1.IdentityServiceClient
	conn   *gogrpc.ClientConn
}

// DialGRPCSessions connects to identity's gRPC API at addr. Plaintext on
// the internal network for now; mTLS between services is Phase 17
// hardening.
func DialGRPCSessions(addr string) (*GRPCSessions, error) {
	conn, err := gogrpc.NewClient(addr, gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("gateway: dialing identity gRPC at %s: %w", addr, err)
	}
	return &GRPCSessions{client: identityv1.NewIdentityServiceClient(conn), conn: conn}, nil
}

// ResolveSession implements SessionResolver.
func (g *GRPCSessions) ResolveSession(ctx context.Context, token string) (string, string, error) {
	resp, err := g.client.ResolveSession(ctx, &identityv1.ResolveSessionRequest{SessionToken: token})
	if status.Code(err) == codes.Unauthenticated {
		return "", "", errInvalidSession
	}
	if err != nil {
		return "", "", fmt.Errorf("gateway: resolving session: %w", err)
	}
	return resp.GetSessionId(), resp.GetUserId(), nil
}

// Close closes the gRPC connection.
func (g *GRPCSessions) Close() error { return g.conn.Close() }
