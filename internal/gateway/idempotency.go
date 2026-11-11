package gateway

import (
	"net/http"
	"strings"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// idempotencyExemptPaths mirrors identity/api/routes.go's own
// skipIdempotencyFor map: RFC 6749's client-credentials grant has to
// work with off-the-shelf OAuth2 client libraries, none of which send
// custom headers. The gateway proxies this path, so it needs the same
// exemption identity's own routes already carry — otherwise the
// gateway would reject a legitimate token request before identity
// ever saw it.
var idempotencyExemptPaths = map[string]bool{
	"/v1/oauth/token": true,
}

// requireIdempotencyKeyHeader rejects a write (.claude/rules/api.md:
// every write needs an Idempotency-Key) with no idempotency.HeaderName
// header, before it's ever forwarded. This is a presence check only —
// not platform/idempotency.Middleware's full claim/replay mechanism,
// which needs a Store and tenant context the gateway has neither of,
// and which identity's own routes already run for real; this is a
// fast-fail at the edge (system design §8: "anything caught before
// acceptance returns an error at once and costs nothing"), defense in
// depth ahead of whatever backend receives the request.
// kycWebhookPrefix is where KYC vendors post results to accounts (task
// 2.6, ADR 0008). A vendor sends no Idempotency-Key; its signature
// authenticates the call and accounts treats a repeated result as a
// no-op. Duplicated from accounts' own exemption, which the gateway
// can't import.
const kycWebhookPrefix = "/v1/accounts/kyc/webhooks/"

// railCallbackPrefix is where payment rails call payments back (task
// 4.4). A rail sends no Idempotency-Key; its source address and path
// token authenticate it, and payments records a repeated callback once.
// Duplicated from payments' own exemption, which the gateway can't
// import.
const railCallbackPrefix = "/v1/payments/callbacks/"

func requireIdempotencyKeyHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if idempotencyExemptPaths[r.URL.Path] || strings.HasPrefix(r.URL.Path, kycWebhookPrefix) || strings.HasPrefix(r.URL.Path, railCallbackPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(idempotency.HeaderName) == "" {
			problem.Respond(w, r, problem.New(problem.VAL4000, "the Idempotency-Key header is required on every write"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
