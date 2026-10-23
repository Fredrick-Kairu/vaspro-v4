package gateway

import (
	"net/http/httputil"
	"net/url"
)

// newReverseProxy builds a plain reverse proxy to target — the gateway
// has exactly one backend in Phase 1 (identity; messaging/payments/etc.
// don't exist yet), so there's no multi-backend registry to build here,
// just the one hop every request not stopped earlier in the pipeline
// ends up taking.
func newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return httputil.NewSingleHostReverseProxy(target)
}
