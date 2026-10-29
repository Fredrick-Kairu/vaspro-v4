package httpx

import (
	"net/http"

	"github.com/google/uuid"

	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"
)

// RequestID assigns a fresh ID to every request, puts it in the context
// under the key platform/log looks for, and echoes it on the X-Request-Id
// response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(vaslog.WithRequestID(r.Context(), id)))
	})
}
