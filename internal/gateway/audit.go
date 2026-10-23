package gateway

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder captures the status code a handler writes, so audit
// logging can report it after the fact — net/http gives no other way
// to observe it from outside the handler.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// audit logs one structured line per request — the gateway's own
// "every credential event is logged" (system design §3), scoped to
// what the gateway itself observes (it owns no outbox/schema to
// publish a real event through). client_id is only present once
// requireJWT has run; routes with no JWT (every production route
// today — see package doc) log without it.
func audit(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			attrs := []slog.Attr{
				slog.String("route", r.URL.Path),
				slog.String("method", r.Method),
				slog.Int("status", rec.status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			}
			if ip, ok := callerIP(r); ok {
				attrs = append(attrs, slog.String("ip", ip.String()))
			}
			if claims, ok := claimsFromContext(r.Context()); ok && claims.ClientID != "" {
				attrs = append(attrs, slog.String("client_id", claims.ClientID))
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "gateway: request", attrs...)
		})
	}
}
