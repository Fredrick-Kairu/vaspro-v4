package gateway

import (
	"net/http"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// requireLiveEnvironment rejects a sandbox credential's token with
// AUTH-4016 (system design §9: "going live changes the credential,
// not the code" — sandbox and live run the same gateway/code, split
// only by the credential's own env). requireJWT must run first.
func requireLiveEnvironment(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := claimsFromContext(r.Context())
		if !ok {
			problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
			return
		}
		if claims.Environment != "live" {
			problem.Respond(w, r, problem.New(problem.AUTH4016, "this route can only be reached with a live credential"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
