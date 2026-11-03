package httpx

import "net/http"

// BodyLimit rejects request bodies larger than maxBytes: reads past the
// limit fail with an error rather than consuming unbounded memory.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimitFunc is BodyLimit with the limit chosen per request, for a
// service where one route takes larger bodies than the rest (document
// uploads, task 2.8).
func BodyLimitFunc(maxBytes func(*http.Request) int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes(r))
			next.ServeHTTP(w, r)
		})
	}
}
