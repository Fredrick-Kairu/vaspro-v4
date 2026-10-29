// Package cache wraps Redis (ROADMAP §6: sessions, rate limits, fast
// state) for whichever service needs it — sessions and rate limiting
// don't exist yet, so this stays a thin wrapper, not a composite
// abstraction over them.
package cache

import "github.com/redis/go-redis/v9"

// Cache is a Redis client. Embedding *redis.Client (rather than wrapping
// it method by method) means every command it supports is already
// available — the same shape platform/db.Pool uses over *pgxpool.Pool.
type Cache struct {
	*redis.Client
}

// New wraps a Redis client configured by opts. Connection isn't verified
// eagerly, consistent with platform/db.NewPool.
func New(opts *redis.Options) *Cache {
	return &Cache{Client: redis.NewClient(opts)}
}
