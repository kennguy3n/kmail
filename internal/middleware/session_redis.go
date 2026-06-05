package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisSessionStore is the shared, cross-replica SessionStore. It is
// required (over MemorySessionStore) whenever KMail runs more than
// one API replica, because the concurrent-session cap and revocation
// must be consistent across every replica a user's requests land on.
//
// Layout:
//
//	sess:<sid>            STRING(JSON SessionInfo), TTL = idle window
//	usess:<userKey>       SET of live sids for the user
//	srevoked:<sid>        STRING("1"), TTL = revoke window
//
// The per-session TTL gives idle expiry for free: a session that is
// not Touched within the idle window is reaped by Valkey, and List
// reconciles the user's SET against the surviving sess: keys.
//
// Redis Cluster note: the Touch and Revoke TxPipelines span keys from
// different namespaces (sess:, usess:, srevoked:) which hash to
// different slots, so they would raise CROSSSLOT on a clustered
// deployment. This is safe today because the store reuses the single
// node *redis.Client backing the rate limiter (not a ClusterClient).
// Moving sessions onto Redis Cluster would require hash-tag alignment
// (e.g. keying everything by {<userKey>}) so a user's keys co-locate.
type RedisSessionStore struct {
	Client *redis.Client
}

// NewRedisSessionStore wraps an existing *redis.Client.
func NewRedisSessionStore(c *redis.Client) *RedisSessionStore {
	return &RedisSessionStore{Client: c}
}

func sessKey(sid string) string    { return "sess:" + sid }
func userSessKey(uk string) string { return "usess:" + uk }
func revokedKey(sid string) string { return "srevoked:" + sid }

// Touch implements SessionStore.
func (s *RedisSessionStore) Touch(ctx context.Context, in SessionInfo, idleTTL time.Duration, maxConcurrent int, now time.Time) ([]string, error) {
	// Preserve CreatedAt across touches: only set it if the session
	// key does not already exist.
	existing, err := s.Client.Get(ctx, sessKey(in.ID)).Bytes()
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("session redis: get: %w", err)
	}
	if err == nil {
		var prev SessionInfo
		if jErr := json.Unmarshal(existing, &prev); jErr == nil {
			in.CreatedAt = prev.CreatedAt
		}
	} else {
		in.CreatedAt = now
	}
	in.LastSeen = now

	payload, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("session redis: marshal: %w", err)
	}

	pipe := s.Client.TxPipeline()
	pipe.Set(ctx, sessKey(in.ID), payload, idleTTL)
	pipe.SAdd(ctx, userSessKey(in.UserKey), in.ID)
	// Keep the index set from outliving its members.
	pipe.Expire(ctx, userSessKey(in.UserKey), idleTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("session redis: touch pipeline: %w", err)
	}

	if maxConcurrent <= 0 {
		return nil, nil
	}
	return s.enforceCap(ctx, in.UserKey, in.ID, maxConcurrent, idleTTL, now)
}

// enforceCap reconciles the user's session set and evicts the oldest
// sessions until at most maxConcurrent remain. `keepID` (the current
// request's session) is never evicted: it is guarded explicitly
// rather than relying solely on it sorting newest-first, so a clock
// skew or an equal CreatedAt timestamp can't evict the caller's own
// just-touched session and lock them out (which would otherwise force
// the middleware down its defensive 401 path). enforceCap is only
// reached with maxConcurrent >= 1, so there are always enough
// non-keepID sessions to bring the count back under the cap.
func (s *RedisSessionStore) enforceCap(ctx context.Context, uk, keepID string, maxConcurrent int, idleTTL time.Duration, now time.Time) ([]string, error) {
	live, err := s.listLive(ctx, uk, idleTTL, now)
	if err != nil {
		return nil, err
	}
	if len(live) <= maxConcurrent {
		return nil, nil
	}
	// listLive returns newest-first; evict from the tail (oldest),
	// skipping keepID, until we're back under the cap.
	excess := len(live) - maxConcurrent
	var evicted []string
	for i := len(live) - 1; i >= 0 && excess > 0; i-- {
		victim := live[i]
		if victim.ID == keepID {
			continue
		}
		pipe := s.Client.TxPipeline()
		pipe.Del(ctx, sessKey(victim.ID))
		pipe.SRem(ctx, userSessKey(uk), victim.ID)
		if _, err := pipe.Exec(ctx); err != nil {
			return evicted, fmt.Errorf("session redis: evict: %w", err)
		}
		evicted = append(evicted, victim.ID)
		excess--
	}
	return evicted, nil
}

// listLive returns the user's live sessions (newest first),
// reconciling the index SET against surviving sess: keys (expired
// members are pruned from the SET).
func (s *RedisSessionStore) listLive(ctx context.Context, uk string, _ time.Duration, _ time.Time) ([]SessionInfo, error) {
	ids, err := s.Client.SMembers(ctx, userSessKey(uk)).Result()
	if err != nil {
		return nil, fmt.Errorf("session redis: smembers: %w", err)
	}
	out := make([]SessionInfo, 0, len(ids))
	var stale []string
	for _, id := range ids {
		raw, err := s.Client.Get(ctx, sessKey(id)).Bytes()
		if err == redis.Nil {
			stale = append(stale, id)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("session redis: get %s: %w", id, err)
		}
		var info SessionInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			stale = append(stale, id)
			continue
		}
		out = append(out, info)
	}
	if len(stale) > 0 {
		// Best-effort prune; ignore error (next call retries).
		// go-redis flattens the []string into individual SREM members
		// (its appendArgs type-asserts slices), so each stale id is
		// removed rather than the slice being treated as one member.
		_ = s.Client.SRem(ctx, userSessKey(uk), stale).Err()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// List implements SessionStore.
func (s *RedisSessionStore) List(ctx context.Context, userKey string, idleTTL time.Duration, now time.Time) ([]SessionInfo, error) {
	return s.listLive(ctx, userKey, idleTTL, now)
}

// Revoke implements SessionStore.
//
// Revocation is authorization-scoped: we only delete the session and
// write a (globally-keyed) revocation tombstone when sessionID is a
// member of the caller's own index set. Otherwise a caller could
// plant a tombstone for any session id they can guess and 401 the
// victim. A non-owned id is a no-op that returns ErrSessionNotFound.
func (s *RedisSessionStore) Revoke(ctx context.Context, userKey, sessionID string, ttl time.Duration, _ time.Time) error {
	owned, err := s.Client.SIsMember(ctx, userSessKey(userKey), sessionID).Result()
	if err != nil {
		return fmt.Errorf("session redis: revoke ownership check: %w", err)
	}
	if !owned {
		return ErrSessionNotFound
	}
	pipe := s.Client.TxPipeline()
	pipe.Del(ctx, sessKey(sessionID))
	pipe.SRem(ctx, userSessKey(userKey), sessionID)
	pipe.Set(ctx, revokedKey(sessionID), "1", ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("session redis: revoke: %w", err)
	}
	return nil
}

// IsRevoked implements SessionStore.
func (s *RedisSessionStore) IsRevoked(ctx context.Context, sessionID string, _ time.Time) (bool, error) {
	n, err := s.Client.Exists(ctx, revokedKey(sessionID)).Result()
	if err != nil {
		return false, fmt.Errorf("session redis: exists: %w", err)
	}
	return n > 0, nil
}
