package gateway

import (
	"net"
	"net/http"
	"net/netip"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// parseIPAllowlist mirrors identity/domain.ParseIPAllowlist's shape
// (CIDR or a bare IP, treated as a single-address prefix) — duplicated
// rather than imported: the gateway must never import a service
// package (CLAUDE.md's boundary), even one as small as this.
func parseIPAllowlist(entries []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			addr, addrErr := netip.ParseAddr(entry)
			if addrErr != nil {
				return nil, addrErr
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

// callerIP is the caller's address: the client a trusted proxy named
// (resolveClientIP), otherwise the connection's peer.
func callerIP(r *http.Request) (netip.Addr, bool) {
	if addr, ok := r.Context().Value(clientIPKey{}).(netip.Addr); ok {
		return addr, true
	}
	return peerIP(r)
}

// peerIP is the address of whatever connected to the gateway.
func peerIP(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// requireIPAllowlist rejects a caller whose IP isn't on the
// credential's own ip_allowlist claim with AUTH-4015. An empty
// allowlist claim means the credential has none configured, so every
// IP is allowed — same "ip_allowlist defaults to empty = unrestricted"
// behavior identity/app.CreateApiClient already has (api_clients, task
// 1.7); only smpp_accounts (task 1.10) ever requires a non-empty one,
// and that's enforced at creation time, not here. requireJWT must run
// first.
func requireIPAllowlist(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := claimsFromContext(r.Context())
		if !ok {
			problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
			return
		}
		if len(claims.IPAllowlist) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		addr, ok := callerIP(r)
		if !ok {
			problem.Respond(w, r, problem.New(problem.AUTH4015, "the caller's IP address could not be determined"))
			return
		}
		prefixes, err := parseIPAllowlist(claims.IPAllowlist)
		if err != nil {
			problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
			return
		}
		for _, p := range prefixes {
			if p.Contains(addr) {
				next.ServeHTTP(w, r)
				return
			}
		}
		problem.Respond(w, r, problem.New(problem.AUTH4015, "the caller's IP address is not on this credential's allowlist"))
	})
}
