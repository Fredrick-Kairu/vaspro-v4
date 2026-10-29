package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// RateLimiter counts requests against a fixed window per key, mirroring
// hmacauth.NonceStore's shape (one small interface, one Redis
// implementation, a fake for unit tests) rather than a bespoke shape
// for this one caller.
type RateLimiter interface {
	// Allow reports whether key may make another request within limit
	// per window. retryAfter is how long the caller should wait before
	// trying again — only meaningful when allowed is false.
	Allow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration, err error)
}

// rateLimitPrefix namespaces rate-limit counters in Redis, same
// reasoning as hmacauth's noncePrefix.
const rateLimitPrefix = "gateway:ratelimit:"

// RedisRateLimiter is a fixed-window counter: the first request in a
// window sets the key with an expiry equal to the window, every
// request after that just increments it. Simpler than a sliding
// window and good enough for Phase 1's "basic abuse protection," not a
// precise quota system — accurate per-client quotas are a billing/
// accounts concern (section 5.1's LIM- family) once that schema
// exists.
type RedisRateLimiter struct {
	cache *cache.Cache
}

// NewRedisRateLimiter wraps c.
func NewRedisRateLimiter(c *cache.Cache) *RedisRateLimiter {
	return &RedisRateLimiter{cache: c}
}

// Allow implements RateLimiter.
func (l *RedisRateLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	redisKey := rateLimitPrefix + key
	count, err := l.cache.Incr(ctx, redisKey).Result()
	if err != nil {
		return false, 0, fmt.Errorf("gateway: incrementing rate limit counter: %w", err)
	}
	if count == 1 {
		if err := l.cache.Expire(ctx, redisKey, window).Err(); err != nil {
			return false, 0, fmt.Errorf("gateway: setting rate limit window: %w", err)
		}
	}
	if count <= int64(limit) {
		return true, 0, nil
	}
	ttl, err := l.cache.TTL(ctx, redisKey).Result()
	if err != nil {
		return false, 0, fmt.Errorf("gateway: reading rate limit ttl: %w", err)
	}
	if ttl < 0 {
		ttl = window
	}
	return false, ttl, nil
}

// rateLimitKeyFunc extracts the key a rate-limit middleware instance
// counts against — e.g. the caller's IP for every request, or the
// JWT's client_id for the protected-route pipeline (requireJWT must
// run first for the latter).
type rateLimitKeyFunc func(r *http.Request) (key string, ok bool)

func rateLimitByIP(r *http.Request) (string, bool) {
	addr, ok := callerIP(r)
	if !ok {
		return "", false
	}
	return "ip:" + addr.String(), true
}

func rateLimitByClientID(r *http.Request) (string, bool) {
	claims, ok := claimsFromContext(r.Context())
	if !ok || claims.ClientID == "" {
		return "", false
	}
	return "client:" + claims.ClientID, true
}

// rateLimit rejects a request over limit-per-window with RATE-4290 and
// a Retry-After header (seconds, rounded up). A key function that
// can't determine a key (e.g. an unparseable RemoteAddr) lets the
// request through rather than failing closed — same reasoning
// requireIPAllowlist's "no allowlist configured" case uses: this is
// abuse protection, not an authorization gate.
func rateLimit(limiter RateLimiter, keyFunc rateLimitKeyFunc, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := keyFunc(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			allowed, retryAfter, err := limiter.Allow(r.Context(), key, limit, window)
			if err != nil {
				problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
				return
			}
			if !allowed {
				seconds := int(retryAfter.Round(time.Second).Seconds())
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				problem.Respond(w, r, problem.New(problem.RATE4290, "rate limit exceeded; retry after the window given in Retry-After"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
