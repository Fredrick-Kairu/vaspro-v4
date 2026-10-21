package httpx

import (
	"context"
	"fmt"
	"net/http"
)

// Healthz reports liveness: the process is up and serving. It never
// checks dependencies — that's Readyz's job — so a slow database can't
// make an orchestrator kill a process that would otherwise recover.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Check is one readiness dependency check.
type Check func(ctx context.Context) error

// Readyz reports readiness: every check passes. These are ops endpoints,
// not part of the versioned API — plain text, not problem+json.
func Readyz(checks ...Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, check := range checks {
			if err := check(r.Context()); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, "not ready: %v", err)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	}
}
