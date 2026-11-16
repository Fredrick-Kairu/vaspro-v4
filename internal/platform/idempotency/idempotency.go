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
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
func Middleware(store Store, ttl time.Duration, opts ...Option) func(http.Handler) http.Handler {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// .claude/rules/api.md requires the header on every WRITE
			// operation — a safe/read method (GET, HEAD, OPTIONS) isn't
			// one, and has no body to deduplicate against anyway. Found
			// by testing: this fell out of a wiring bug once identity's
			// first GET endpoint (1.4's "list passkeys") hit this
			// middleware and got rejected for lacking a header its own
			// OpenAPI contract never asked a reader to send.
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

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

			_ = store.Complete(r.Context(), tenantID, key, buf.code, o.stored(r, buf.code, buf.body.Bytes()))
		})
	}
}

// Option configures Middleware.
type Option func(*options)

type options struct {
	redactions []redaction
}

type redaction struct {
	match  func(*http.Request) bool
	fields []string
}

// WithRedactedFields keeps one-time secrets out of what Middleware
// stores: for a request match accepts, a 2xx response that is a JSON
// object is stored with these fields (top-level names, or dotted paths
// into nested objects) set to null. The live
// response is sent unchanged; a replay of the same key gets the stored
// copy, so it creates nothing twice and never shows the secret again.
func WithRedactedFields(match func(*http.Request) bool, fields ...string) Option {
	return func(o *options) { o.redactions = append(o.redactions, redaction{match: match, fields: fields}) }
}

// stored is what Middleware keeps for r's response: the body, with any
// redactions applied.
func (o options) stored(r *http.Request, code int, body []byte) []byte {
	if code < 200 || code > 299 {
		return body
	}
	for _, rd := range o.redactions {
		if rd.match(r) {
			body = nullFields(body, rd.fields)
		}
	}
	return body
}

// nullFields sets fields to null in a JSON object, or returns body as it
// is if it isn't one or has none of them. A field may be a dotted path
// into nested objects ("secret.secret"); a plain name is a top-level
// field only.
func nullFields(body []byte, fields []string) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	for _, f := range fields {
		if nullPath(obj, strings.Split(f, ".")) {
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// nullPath sets the field at path to null, reporting whether it found
// it.
func nullPath(obj map[string]json.RawMessage, path []string) bool {
	raw, ok := obj[path[0]]
	if !ok {
		return false
	}
	if len(path) == 1 {
		obj[path[0]] = json.RawMessage("null")
		return true
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw, &inner); err != nil || !nullPath(inner, path[1:]) {
		return false
	}
	b, err := json.Marshal(inner)
	if err != nil {
		return false
	}
	obj[path[0]] = b
	return true
}
