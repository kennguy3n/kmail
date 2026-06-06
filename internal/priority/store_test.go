package priority

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/kennguy3n/kmail/internal/smartfeatures"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func scored(id string, score int) Scored {
	return Scored{Message: smartfeatures.Message{ID: id}, Score: score}
}

func TestStore_SaveAndTop(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(newTestRedis(t), time.Minute)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Save(ctx, "t1", "u1", []Scored{scored("E1", 90), scored("E2", 50), scored("E3", 70)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	top, err := s.Top(ctx, "t1", "u1", 2)
	if err != nil {
		t.Fatalf("Top: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("expected 2, got %d", len(top))
	}
	if top[0].EmailID != "E1" || top[0].Score != 90 {
		t.Fatalf("top[0] = %#v", top[0])
	}
	if top[1].EmailID != "E3" {
		t.Fatalf("top[1] = %#v (want E3)", top[1])
	}
}

func TestStore_SaveReplacesAtomically(t *testing.T) {
	ctx := context.Background()
	s, _ := NewStore(newTestRedis(t), time.Minute)
	_ = s.Save(ctx, "t1", "u1", []Scored{scored("OLD", 90)})
	_ = s.Save(ctx, "t1", "u1", []Scored{scored("NEW", 10)})
	top, _ := s.Top(ctx, "t1", "u1", 10)
	if len(top) != 1 || top[0].EmailID != "NEW" {
		t.Fatalf("expected only NEW after replace, got %#v", top)
	}
}

func TestStore_EmptyClears(t *testing.T) {
	ctx := context.Background()
	s, _ := NewStore(newTestRedis(t), time.Minute)
	_ = s.Save(ctx, "t1", "u1", []Scored{scored("E1", 90)})
	_ = s.Save(ctx, "t1", "u1", nil)
	top, _ := s.Top(ctx, "t1", "u1", 10)
	if len(top) != 0 {
		t.Fatalf("expected cleared, got %#v", top)
	}
}

func TestStore_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	s, _ := NewStore(newTestRedis(t), time.Minute)
	_ = s.Save(ctx, "t1", "u1", []Scored{scored("E1", 90)})
	top, _ := s.Top(ctx, "t2", "u1", 10)
	if len(top) != 0 {
		t.Fatalf("expected tenant isolation, got %#v", top)
	}
}
