package httpx

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// Timeout bounds how long next may run. A handler that doesn't finish
// within d gets a SYS-5030 problem+json response instead of whatever it
// was about to write — its write is discarded, same as net/http's own
// TimeoutHandler, but with a problem+json body instead of a fixed
// plain-text one (which is the one thing TimeoutHandler can't be
// configured to avoid).
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			r = r.WithContext(ctx)

			tw := &timeoutWriter{header: make(http.Header)}
			done := make(chan struct{})
			panicChan := make(chan any, 1)

			go func() {
				defer func() {
					if p := recover(); p != nil {
						panicChan <- p
					}
				}()
				next.ServeHTTP(tw, r)
				close(done)
			}()

			select {
			case p := <-panicChan:
				panic(p)
			case <-done:
				tw.mu.Lock()
				defer tw.mu.Unlock()
				maps.Copy(w.Header(), tw.header)
				if !tw.wroteHeader {
					tw.code = http.StatusOK
				}
				w.WriteHeader(tw.code)
				_, _ = w.Write(tw.buf.Bytes())
			case <-ctx.Done():
				tw.mu.Lock()
				tw.timedOut = true
				tw.mu.Unlock()
				problem.Respond(w, r, problem.New(problem.SYS5030, "The request didn't finish in time."))
			}
		})
	}
}

// timeoutWriter buffers a handler's response until Timeout decides whether
// the handler finished in time. Writes after timedOut is set are silently
// dropped — the handler's goroutine may still be running when that
// happens, and net/http gives no way to forcibly stop it.
type timeoutWriter struct {
	header http.Header
	buf    bytes.Buffer

	mu          sync.Mutex
	wroteHeader bool
	timedOut    bool
	code        int
}

func (tw *timeoutWriter) Header() http.Header { return tw.header }

func (tw *timeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.timedOut {
		return len(p), nil
	}
	if !tw.wroteHeader {
		tw.writeHeaderLocked(http.StatusOK)
	}
	return tw.buf.Write(p)
}

func (tw *timeoutWriter) WriteHeader(code int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.writeHeaderLocked(code)
}

func (tw *timeoutWriter) writeHeaderLocked(code int) {
	if tw.timedOut || tw.wroteHeader {
		return
	}
	tw.wroteHeader = true
	tw.code = code
}
