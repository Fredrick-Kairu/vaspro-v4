package hmacauth

import (
	"context"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
)

// noncePrefix namespaces nonce keys in Redis so they can't collide
// with anything else a service might store in the same cache.
const noncePrefix = "hmacauth:nonce:"

// RedisNonceStore is NonceStore's one concrete implementation —
// Redis's SETNX gives the atomic claim-once semantics Claim needs for
// free.
type RedisNonceStore struct {
	cache *cache.Cache
}

// NewRedisNonceStore wraps c.
func NewRedisNonceStore(c *cache.Cache) *RedisNonceStore {
	return &RedisNonceStore{cache: c}
}

// Claim implements NonceStore.
func (s *RedisNonceStore) Claim(ctx context.Context, nonce string, ttl time.Duration) (bool, error) {
	return s.cache.SetNX(ctx, noncePrefix+nonce, "1", ttl).Result()
}
