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
// sessions until at most maxConcurrent remain. It never evicts
// `keepID` first (the current request's session) — they are evicted
// by age, and the current session is the newest.
func (s *RedisSessionStore) enforceCap(ctx context.Context, uk, keepID string, maxConcurrent int, idleTTL time.Duration, now time.Time) ([]string, error) {
	live, err := s.listLive(ctx, uk, idleTTL, now)
	if err != nil {
		return nil, err
	}
	if len(live) <= maxConcurrent {
		return nil, nil
	}
	// listLive returns newest-first; evict from the tail (oldest).
	var evicted []string
	for _, victim := range live[maxConcurrent:] {
		pipe := s.Client.TxPipeline()
		pipe.Del(ctx, sessKey(victim.ID))
		pipe.SRem(ctx, userSessKey(uk), victim.ID)
		if _, err := pipe.Exec(ctx); err != nil {
			return evicted, fmt.Errorf("session redis: evict: %w", err)
		}
		evicted = append(evicted, victim.ID)
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
func (s *RedisSessionStore) Revoke(ctx context.Context, userKey, sessionID string, ttl time.Duration, _ time.Time) error {
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
