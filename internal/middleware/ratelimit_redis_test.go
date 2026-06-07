package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRateLimitStore(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisStoreFromClient(client), mr
}

func TestRedisStoreAllow(t *testing.T) {
	store, _ := newRateLimitStore(t)
	ctx := context.Background()
	now := time.Now()
	window := time.Minute

	// First call within limits: both allowed.
	tenantOK, userOK, err := store.Allow(ctx, "t:tenant1", "u:user1", window, 5, 3, now)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !tenantOK || !userOK {
		t.Fatalf("first call tenantOK=%v userOK=%v want both true", tenantOK, userOK)
	}

	// Exhaust the user limit (userLimit=3). After 3 total calls the
	// 4th should report userOK=false.
	for i := 0; i < 2; i++ {
		if _, _, err := store.Allow(ctx, "t:tenant1", "u:user1", window, 100, 3, now.Add(time.Duration(i+1)*time.Millisecond)); err != nil {
			t.Fatalf("Allow loop: %v", err)
		}
	}
	_, userOK, err = store.Allow(ctx, "t:tenant1", "u:user1", window, 100, 3, now.Add(10*time.Millisecond))
	if err != nil {
		t.Fatalf("Allow over-limit: %v", err)
	}
	if userOK {
		t.Error("expected userOK=false after exceeding user limit")
	}
}

func TestRedisStoreAllowNilClient(t *testing.T) {
	s := &RedisStore{}
	if _, _, err := s.Allow(context.Background(), "t", "u", time.Minute, 1, 1, time.Now()); err == nil {
		t.Error("Allow with nil client must error")
	}
}

func TestRedisStoreIncrWithTTL(t *testing.T) {
	store, _ := newRateLimitStore(t)
	ctx := context.Background()

	n1, err := store.IncrWithTTL(ctx, "counter:a", time.Minute)
	if err != nil {
		t.Fatalf("IncrWithTTL: %v", err)
	}
	if n1 != 1 {
		t.Fatalf("first incr=%d want 1", n1)
	}
	n2, err := store.IncrWithTTL(ctx, "counter:a", time.Minute)
	if err != nil || n2 != 2 {
		t.Fatalf("second incr=%d err=%v want 2", n2, err)
	}

	// Non-positive TTL is rejected.
	if _, err := store.IncrWithTTL(ctx, "counter:a", 0); err == nil {
		t.Error("IncrWithTTL with zero ttl must error")
	}
}
