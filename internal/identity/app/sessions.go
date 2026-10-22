package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// sessionCacheKey is where ResolveSession looks a session up in Redis.
func sessionCacheKey(sessionID uuid.UUID) string {
	return "identity:session:" + sessionID.String()
}

// ResolveSession is the per-request bearer-token check (requireSession's
// stand-in for the real gateway's check, task 1.11) — Redis first, then
// Postgres on a miss. Populating the cache only lazily, on a Postgres
// hit, rather than write-through at createSession time, avoids a tiny
// but real race: createSession runs inside its caller's transaction, so
// a write-through cache entry could validate a session whose insert
// later rolls back. The cost is one guaranteed miss on the very first
// request after sign-in, which is negligible.
//
// last_seen_at is bumped only on the Postgres path, not on every cache
// hit — updating it on every authenticated request would turn the
// cache into a write-amplifier and defeat its own purpose.
func (s *Service) ResolveSession(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, error) {
	key := sessionCacheKey(sessionID)
	if s.cache != nil {
		cached, err := s.cache.Get(ctx, key).Result()
		if err == nil {
			userID, err := uuid.Parse(cached)
			if err == nil {
				return userID, nil
			}
		}
		// A miss (redis.Nil) or any other cache error both fall through
		// to Postgres — the cache is a speedup, not a new point of
		// failure for authentication.
	}

	q := store.New(s.pool)
	session, err := q.GetActiveSessionByID(ctx, store.GetActiveSessionByIDParams{
		ID:  sessionID,
		Now: s.clk.Now(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: looking up session: %w", err)
	}

	if err := q.UpdateSessionLastSeenAt(ctx, sessionID); err != nil {
		return uuid.Nil, fmt.Errorf("app: updating session last-seen: %w", err)
	}

	if s.cache != nil {
		ttl := session.ExpiresAt.Sub(s.clk.Now())
		if ttl > 0 {
			_ = s.cache.Set(ctx, key, session.UserID.String(), ttl).Err()
		}
	}

	return session.UserID, nil
}

// evictSession removes sessionID from the Redis cache, regardless of
// whether it was ever cached — called by every revoke path so a
// revoked session can never still validate from a stale entry.
func (s *Service) evictSession(ctx context.Context, sessionID uuid.UUID) {
	if s.cache == nil {
		return
	}
	_ = s.cache.Del(ctx, sessionCacheKey(sessionID)).Err()
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
	n, err := store.New(s.pool).RevokeSession(ctx, store.RevokeSessionParams{ID: sessionID, UserID: userID})
	if err != nil {
		return fmt.Errorf("app: revoking session: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	s.evictSession(ctx, sessionID)
	return nil
}

// SignOutEverywhere revokes every one of userID's active sessions,
// including the one the caller is using right now — the caller must
// sign in again afterward. Returns the number of sessions revoked.
func (s *Service) SignOutEverywhere(ctx context.Context, userID uuid.UUID) (int, error) {
	ids, err := store.New(s.pool).RevokeAllSessions(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("app: revoking all sessions: %w", err)
	}
	for _, id := range ids {
		s.evictSession(ctx, id)
	}
	return len(ids), nil
}
