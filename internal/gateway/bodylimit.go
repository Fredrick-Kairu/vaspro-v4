package gateway

import (
	"net/http"
	"strings"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// Request bodies are limited to 1 MB, except accounts' document upload
// (task 2.8), which carries a file of up to 10 MB plus its multipart
// framing. Mirrors accounts' own limits (accounts/api/routes.go), which
// the gateway can't import.
const (
	defaultBodyLimit = 1 << 20
	uploadBodyLimit  = 10<<20 + 1<<20
)

func bodyLimit(r *http.Request) int64 {
	if isDocumentUpload(r) {
		return uploadBodyLimit
	}
	return defaultBodyLimit
}

func isDocumentUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/accounts/tenants/") && strings.HasSuffix(r.URL.Path, "/documents")
}

// refuseOversizedUploads answers an upload whose declared length is over
// its limit with VAL-4008 before anything reads it, rather than the
// generic validation error a cut-off body would get.
func refuseOversizedUploads(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isDocumentUpload(r) && r.ContentLength > uploadBodyLimit {
			problem.Respond(w, r, problem.New(problem.VAL4008, "files are limited to 10 MB"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
