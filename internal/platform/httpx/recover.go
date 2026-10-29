package httpx

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// Recover turns a panic in next into a SYS-9000 problem+json response.
// The panic value and a stack trace go to logger, under the request's
// context (so they carry request_id/trace_id) — never into the response
// body (system design §8).
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.ErrorContext(r.Context(), "panic recovered",
						slog.Any("panic", rec),
						slog.String("stack", string(debug.Stack())),
					)
					problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
