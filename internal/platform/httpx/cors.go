package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORSConfig is a strict allow-list: there is no wildcard option on
// purpose. An origin not in AllowedOrigins gets no CORS headers at all,
// so the browser blocks it.
type CORSConfig struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	MaxAge         time.Duration
}

// CORS enforces cfg: only a listed origin gets Access-Control-Allow-Origin
// (echoed back, never "*") plus Vary: Origin; an OPTIONS preflight from a
// listed origin gets the configured methods/headers and a 204.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[o] = true
	}
	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			isAllowed := origin != "" && allowed[origin]

			if isAllowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}

			if r.Method == http.MethodOptions {
				if isAllowed {
					w.Header().Set("Access-Control-Allow-Methods", methods)
					w.Header().Set("Access-Control-Allow-Headers", headers)
					if cfg.MaxAge > 0 {
						w.Header().Set("Access-Control-Max-Age", strconv.Itoa(int(cfg.MaxAge.Seconds())))
					}
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
