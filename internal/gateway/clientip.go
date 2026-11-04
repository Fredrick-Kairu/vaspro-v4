package gateway

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
)

type clientIPKey struct{}

// resolveClientIP decides who the caller is when the gateway sits behind
// a reverse proxy (the dev environment's Caddy). Only when the direct
// peer is one of trusted does it read X-Forwarded-For, right to left,
// skipping trusted hops: the first address no trusted hop could have
// added is the client. With no trusted proxies, or an untrusted peer,
// the peer itself is the caller and the header is ignored, so a client
// can't choose its own address. callerIP reads the result.
func resolveClientIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if addr, ok := forwardedClient(r, trusted); ok {
				r = r.WithContext(context.WithValue(r.Context(), clientIPKey{}, addr))
			}
			next.ServeHTTP(w, r)
		})
	}
}

func forwardedClient(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	peer, ok := peerIP(r)
	if !ok || !isTrusted(peer, trusted) {
		return netip.Addr{}, false
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := netip.Addr{}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return netip.Addr{}, false
		}
		client = addr.Unmap()
		if !isTrusted(client, trusted) {
			break
		}
	}
	return client, client.IsValid()
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
