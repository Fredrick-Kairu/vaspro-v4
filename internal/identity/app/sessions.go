package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// sessionCacheKey is where ResolveSession looks a session up in Redis:
// keyed by the token's hash (never the raw token, and never the
// session id, which isn't a credential).
func sessionCacheKey(tokenHash []byte) string {
	return "identity:session:" + hex.EncodeToString(tokenHash)
}

// ResolveSession is the per-request bearer-token check (requireSession's
// stand-in for the real gateway's check, task 1.11): token is the
// session token sign-in returned, never the session id. Redis first,
// then Postgres on a miss. Populating the cache only lazily, on a
// Postgres hit, rather than write-through at createSession time, avoids
// a tiny but real race: createSession runs inside its caller's
// transaction, so a write-through cache entry could validate a session
// whose insert later rolls back. The cost is one guaranteed miss on the
// very first request after sign-in, which is negligible.
//
// last_seen_at is bumped only on the Postgres path, not on every cache
// hit — updating it on every authenticated request would turn the
// cache into a write-amplifier and defeat its own purpose.
func (s *Service) ResolveSession(ctx context.Context, token string) (sessionID, userID uuid.UUID, err error) {
	tokenHash := domain.HashSessionToken(token)
	key := sessionCacheKey(tokenHash)
	if s.cache != nil {
		cached, err := s.cache.Get(ctx, key).Result()
		if err == nil {
			if sessionID, userID, ok := parseCachedSession(cached); ok {
				return sessionID, userID, nil
			}
		}
		// A miss (redis.Nil) or any other cache error both fall through
		// to Postgres — the cache is a speedup, not a new point of
		// failure for authentication.
	}

	q := store.New(s.pool)
	session, err := q.GetActiveSessionByTokenHash(ctx, store.GetActiveSessionByTokenHashParams{
		TokenHash: tokenHash,
		Now:       s.clk.Now(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("app: looking up session: %w", err)
	}

	if err := q.UpdateSessionLastSeenAt(ctx, session.ID); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("app: updating session last-seen: %w", err)
	}

	if s.cache != nil {
		ttl := session.ExpiresAt.Sub(s.clk.Now())
		if ttl > 0 {
			_ = s.cache.Set(ctx, key, session.ID.String()+" "+session.UserID.String(), ttl).Err()
		}
	}

	return session.ID, session.UserID, nil
}

// parseCachedSession reads the "<session id> <user id>" value
// ResolveSession caches; anything else is treated as a miss.
func parseCachedSession(v string) (sessionID, userID uuid.UUID, ok bool) {
	sid, uid, found := strings.Cut(v, " ")
	if !found {
		return uuid.Nil, uuid.Nil, false
	}
	sessionID, err := uuid.Parse(sid)
	if err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	userID, err = uuid.Parse(uid)
	if err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	return sessionID, userID, true
}

// evictSession removes a session's Redis cache entry, regardless of
// whether it was ever cached — called by every revoke path so a
// revoked session can never still validate from a stale entry. A nil
// tokenHash (a row from before sessions had tokens) was never cached.
func (s *Service) evictSession(ctx context.Context, tokenHash []byte) {
	if s.cache == nil || tokenHash == nil {
		return
	}
	_ = s.cache.Del(ctx, sessionCacheKey(tokenHash)).Err()
}

// SessionSummary is one of a user's active sessions, as shown to its
// owner.
type SessionSummary struct {
	ID         uuid.UUID
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	Current    bool
	// AuthMethod is how the session signed in: password, passkey or
	// google (sessions.auth_method, task 1.16).
	AuthMethod string
}

// ListSessions returns userID's active sessions (revoked or expired
// ones aren't shown — this is a device-management view, not a history
// log), most recently seen first. currentSessionID flags which row (if
// any) is the one the caller is using right now.
func (s *Service) ListSessions(ctx context.Context, userID, currentSessionID uuid.UUID) ([]SessionSummary, error) {
	rows, err := store.New(s.pool).ListActiveSessionsByUserID(ctx, store.ListActiveSessionsByUserIDParams{
		UserID: userID,
		Now:    s.clk.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("app: listing sessions: %w", err)
	}

	summaries := make([]SessionSummary, len(rows))
	for i, row := range rows {
		summary := SessionSummary{
			ID:         row.ID,
			CreatedAt:  row.CreatedAt,
			LastSeenAt: row.LastSeenAt,
			ExpiresAt:  row.ExpiresAt,
			Current:    row.ID == currentSessionID,
			AuthMethod: row.AuthMethod,
		}
		if row.Ip != nil {
			summary.IP = row.Ip.String()
		}
		if row.UserAgent.Valid {
			summary.UserAgent = row.UserAgent.String
		}
		summaries[i] = summary
	}
	return summaries, nil
}

// RevokeSession revokes sessionID, which must belong to userID. Neither
// "doesn't exist" nor "exists but isn't yours" is distinguished — both
// return ErrNotFound, the same ownership-check shape 1.4 built for
// passkeys.
func (s *Service) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	tokenHash, err := store.New(s.pool).RevokeSession(ctx, store.RevokeSessionParams{ID: sessionID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("app: revoking session: %w", err)
	}
	s.evictSession(ctx, tokenHash)
	return nil
}

// SignOutEverywhere revokes every one of userID's active sessions,
// including the one the caller is using right now — the caller must
// sign in again afterward. Returns the number of sessions revoked.
func (s *Service) SignOutEverywhere(ctx context.Context, userID uuid.UUID) (int, error) {
	tokenHashes, err := store.New(s.pool).RevokeAllSessions(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("app: revoking all sessions: %w", err)
	}
	for _, tokenHash := range tokenHashes {
		s.evictSession(ctx, tokenHash)
	}
	return len(tokenHashes), nil
}
