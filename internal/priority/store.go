package priority

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store caches per-user priority scores in Valkey. Scores are
// ephemeral by design — they are a derived ranking that can always
// be recomputed from mail + send history, so the keys carry a TTL
// and losing them on eviction is harmless.
//
//	kmail:priority:<tenant>:<user>  ZSET  score=priority 0..100, member=email id
type Store struct {
	client redis.UniversalClient
	ttl    time.Duration
}

// DefaultScoreTTL bounds how long a cached ranking lives before a
// recompute is required. Short enough that the ranking tracks a
// shifting inbox, long enough to absorb repeated reads.
const DefaultScoreTTL = 15 * time.Minute

// NewStore constructs the score cache. ttl <= 0 falls back to
// DefaultScoreTTL.
func NewStore(client redis.UniversalClient, ttl time.Duration) (*Store, error) {
	if client == nil {
		return nil, errors.New("priority.NewStore: client is required")
	}
	if ttl <= 0 {
		ttl = DefaultScoreTTL
	}
	return &Store{client: client, ttl: ttl}, nil
}

func scoreKey(tenantID, userID string) string {
	return fmt.Sprintf("kmail:priority:%s:%s", tenantID, userID)
}

// Save replaces the cached ranking for a user atomically: the old
// set is dropped and the new scores written in one pipeline so a
// reader never observes a half-updated ranking. An empty scored
// list still clears the key (the user's inbox may have emptied).
func (s *Store) Save(ctx context.Context, tenantID, userID string, scored []Scored) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" {
		return errors.New("priority.Store.Save: tenantID and userID are required")
	}
	key := scoreKey(tenantID, userID)
	pipe := s.client.TxPipeline()
	pipe.Del(ctx, key)
	if len(scored) > 0 {
		members := make([]redis.Z, 0, len(scored))
		for _, sc := range scored {
			if sc.Message.ID == "" {
				continue
			}
			members = append(members, redis.Z{Score: float64(sc.Score), Member: sc.Message.ID})
		}
		if len(members) > 0 {
			pipe.ZAdd(ctx, key, members...)
			pipe.Expire(ctx, key, s.ttl)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("priority.Store.Save: %w", err)
	}
	return nil
}

// ScoredID is a cached (email id, score) pair.
type ScoredID struct {
	EmailID string `json:"email_id"`
	Score   int    `json:"score"`
}

// Top returns the highest-scored email ids for a user, descending.
// n <= 0 returns an empty slice. A cache miss (no key) returns an
// empty slice with no error so the handler can decide to recompute.
func (s *Store) Top(ctx context.Context, tenantID, userID string, n int) ([]ScoredID, error) {
	if n <= 0 {
		return []ScoredID{}, nil
	}
	res, err := s.client.ZRevRangeWithScores(ctx, scoreKey(tenantID, userID), 0, int64(n-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("priority.Store.Top: %w", err)
	}
	out := make([]ScoredID, 0, len(res))
	for _, z := range res {
		member, _ := z.Member.(string)
		out = append(out, ScoredID{EmailID: member, Score: int(z.Score)})
	}
	return out, nil
}
