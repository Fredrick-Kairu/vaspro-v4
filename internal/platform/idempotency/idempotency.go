// Package idempotency makes a write endpoint safe to retry: the same
// Idempotency-Key with the same body replays the first response instead
// of running the handler again (data model §1's idempotency_keys,
// .claude/rules/api.md's "every write operation requires an
// Idempotency-Key header").
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// HeaderName is the request header clients must send on every write.
const HeaderName = "Idempotency-Key"

// Record is a previously claimed key, as returned by Store.Claim.
type Record struct {
	RequestHash []byte
	StatusCode  int
	Response    []byte
	// Complete is false if the key was claimed but Store.Complete hasn't
	// been called for it yet — a request for this key is (or was, if its
	// process died) still in flight. See the package doc for how
	// Middleware handles this.
	Complete bool
}

// Store persists idempotency records.
type Store interface {
	// Claim atomically creates a pending record for (tenantID, key) with
	// requestHash if none exists (or the existing one has expired), and
	// returns nil, nil in that case — the caller owns this key now and
	// should run the handler. If a live record already exists, Claim
	// returns it unchanged instead of creating a second one.
	Claim(ctx context.Context, tenantID uuid.UUID, key string, requestHash []byte, expiresAt time.Time) (existing *Record, err error)

	// Complete saves the response for a key this caller previously
	// claimed (Claim returned nil, nil for it).
	Complete(ctx context.Context, tenantID uuid.UUID, key string, statusCode int, response []byte) error
}

// bufferedResponseWriter captures a handler's response so Middleware can
// both send it to the real client and hand it to Store.Complete. Unlike
// httpx.Timeout's buffering writer, this runs synchronously in the
// request's own goroutine, so it needs no locking.
type bufferedResponseWriter struct {
	header      http.Header
	body        bytes.Buffer
	code        int
	wroteHeader bool
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{header: make(http.Header)}
}

func (b *bufferedResponseWriter) Header() http.Header { return b.header }

func (b *bufferedResponseWriter) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	return b.body.Write(p)
}

func (b *bufferedResponseWriter) WriteHeader(code int) {
	if b.wroteHeader {
		return
	}
	b.wroteHeader = true
	b.code = code
}

// Middleware requires HeaderName on every request it wraps. A request
// without it is rejected before the handler runs.
//
// Known limitation: two concurrent requests racing on the same key both
// see Claim return nil (the first) or a Record with Complete=false (the
// second, since the first hasn't finished yet). This middleware lets the
// second one run the handler again rather than blocking or polling for
// the first to finish — acceptable for now because nothing in this
// codebase yet issues concurrent duplicate requests on purpose; revisit
// if that changes.
func Middleware(store Store, ttl time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(HeaderName)
			if key == "" {
				problem.Respond(w, r, problem.New(problem.VAL4000, "the "+HeaderName+" header is required"))
				return
			}

			tenantIDStr, ok := vaslog.TenantID(r.Context())
			if !ok {
				problem.Respond(w, r, problem.New(problem.SYS9000, "no tenant in request context"))
				return
			}
			tenantID, err := uuid.Parse(tenantIDStr)
			if err != nil {
				problem.Respond(w, r, problem.New(problem.SYS9000, "tenant in request context is not a valid uuid"))
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				problem.Respond(w, r, problem.New(problem.VAL4000, "could not read the request body"))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			hash := sha256.Sum256(body)

			existing, err := store.Claim(r.Context(), tenantID, key, hash[:], time.Now().Add(ttl))
			if err != nil {
				problem.Respond(w, r, problem.New(problem.SYS9000, "idempotency store error"))
				return
			}
			if existing != nil {
				if !bytes.Equal(existing.RequestHash, hash[:]) {
					problem.Respond(w, r, problem.New(problem.VAL4001, "the "+HeaderName+" header was already used with a different request body"))
					return
				}
				if existing.Complete {
					// idempotency_keys has no column for headers (data
					// model §1) — a replay can't restore the original
					// Content-Type exactly, so it defaults to the one
					// every response in this codebase actually is.
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(existing.StatusCode)
					_, _ = w.Write(existing.Response) //nolint:gosec // replaying our own previously-stored JSON response, not reflecting new user input
					return
				}
				// Pending: fall through and run the handler (see the
				// documented limitation above).
			}

			buf := newBufferedResponseWriter()
			next.ServeHTTP(buf, r)

			for k, vv := range buf.header {
				w.Header()[k] = vv
			}
			if !buf.wroteHeader {
				buf.WriteHeader(http.StatusOK)
			}
			w.WriteHeader(buf.code)
			_, _ = w.Write(buf.body.Bytes())

			_ = store.Complete(r.Context(), tenantID, key, buf.code, buf.body.Bytes())
		})
	}
}
