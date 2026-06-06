package smartfeatures

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// UnsubscribeStore records, per user, the mailing lists they have
// unsubscribed from. It is a thin Valkey-set wrapper: the UI uses
// it to flip the "Unsubscribe" button to "Unsubscribed" without
// re-deriving state from the list operator, and to suppress the
// affordance on future messages from the same list.
//
//	kmail:unsubscribed:<tenant>:<user>  SET  members=list id
type UnsubscribeStore struct {
	client redis.UniversalClient
	ttl    time.Duration
}

// DefaultUnsubscribeTTL keeps an unsubscribe record long enough to
// outlive a sender's send cadence. Unlike contact frequency this
// is closer to a system of record, so the TTL is generous (1y);
// the authoritative state still lives with the list operator.
const DefaultUnsubscribeTTL = 365 * 24 * time.Hour

// NewUnsubscribeStore constructs the store. ttl <= 0 falls back to
// DefaultUnsubscribeTTL.
func NewUnsubscribeStore(client redis.UniversalClient, ttl time.Duration) (*UnsubscribeStore, error) {
	if client == nil {
		return nil, errors.New("smartfeatures.NewUnsubscribeStore: client is required")
	}
	if ttl <= 0 {
		ttl = DefaultUnsubscribeTTL
	}
	return &UnsubscribeStore{client: client, ttl: ttl}, nil
}

func unsubKey(tenantID, userID string) string {
	return fmt.Sprintf("kmail:unsubscribed:%s:%s", tenantID, userID)
}

// Mark records that the user unsubscribed from listID. Idempotent.
func (s *UnsubscribeStore) Mark(ctx context.Context, tenantID, userID, listID string) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" {
		return errors.New("smartfeatures.UnsubscribeStore.Mark: tenantID and userID are required")
	}
	listID = strings.ToLower(strings.TrimSpace(listID))
	if listID == "" {
		return errors.New("smartfeatures.UnsubscribeStore.Mark: listID is required")
	}
	key := unsubKey(tenantID, userID)
	pipe := s.client.Pipeline()
	pipe.SAdd(ctx, key, listID)
	pipe.Expire(ctx, key, s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("smartfeatures.UnsubscribeStore.Mark: %w", err)
	}
	return nil
}

// IsUnsubscribed reports whether the user already unsubscribed from
// listID. A blank listID is treated as "not unsubscribed" rather
// than an error so callers can pass a possibly-empty derived id.
func (s *UnsubscribeStore) IsUnsubscribed(ctx context.Context, tenantID, userID, listID string) (bool, error) {
	listID = strings.ToLower(strings.TrimSpace(listID))
	if listID == "" {
		return false, nil
	}
	ok, err := s.client.SIsMember(ctx, unsubKey(tenantID, userID), listID).Result()
	if err != nil {
		return false, fmt.Errorf("smartfeatures.UnsubscribeStore.IsUnsubscribed: %w", err)
	}
	return ok, nil
}

// List returns every list the user has unsubscribed from.
func (s *UnsubscribeStore) List(ctx context.Context, tenantID, userID string) ([]string, error) {
	res, err := s.client.SMembers(ctx, unsubKey(tenantID, userID)).Result()
	if err != nil {
		return nil, fmt.Errorf("smartfeatures.UnsubscribeStore.List: %w", err)
	}
	return res, nil
}
