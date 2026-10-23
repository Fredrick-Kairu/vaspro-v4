package gateway

import (
	"net/http"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// requireScope rejects a request whose Claims (requireJWT must run
// first in the chain) don't include scope with AUTH-4014 — an empty
// scope means the route needs no particular scope, just a valid JWT.
func requireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := claimsFromContext(r.Context())
			if !ok {
				problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
				return
			}
			if !claims.hasScope(scope) {
				problem.Respond(w, r, problem.New(problem.AUTH4014, "this route requires a scope the credential doesn't have"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
