package smartfeatures

import (
	"context"
	"testing"
	"time"
)

func TestUnsubscribeStore(t *testing.T) {
	ctx := context.Background()
	s, err := NewUnsubscribeStore(newTestRedis(t), time.Hour)
	if err != nil {
		t.Fatalf("NewUnsubscribeStore: %v", err)
	}

	done, err := s.IsUnsubscribed(ctx, "t1", "u1", "list.example.com")
	if err != nil {
		t.Fatalf("IsUnsubscribed: %v", err)
	}
	if done {
		t.Fatalf("expected not unsubscribed initially")
	}

	if err := s.Mark(ctx, "t1", "u1", "List.Example.com"); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	// Case-insensitive match.
	done, _ = s.IsUnsubscribed(ctx, "t1", "u1", "list.example.com")
	if !done {
		t.Fatalf("expected unsubscribed after Mark")
	}

	// Tenant isolation.
	done, _ = s.IsUnsubscribed(ctx, "t2", "u1", "list.example.com")
	if done {
		t.Fatalf("unsubscribe leaked across tenants")
	}

	list, err := s.List(ctx, "t1", "u1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0] != "list.example.com" {
		t.Fatalf("List = %#v", list)
	}
}

func TestUnsubscribeStore_Validations(t *testing.T) {
	ctx := context.Background()
	s, _ := NewUnsubscribeStore(newTestRedis(t), time.Hour)
	if err := s.Mark(ctx, "", "u1", "l"); err == nil {
		t.Fatalf("expected error for empty tenant")
	}
	if err := s.Mark(ctx, "t1", "u1", "  "); err == nil {
		t.Fatalf("expected error for blank list id")
	}
	// Blank list id on read is "not unsubscribed", not an error.
	done, err := s.IsUnsubscribed(ctx, "t1", "u1", "")
	if err != nil || done {
		t.Fatalf("blank list id read: done=%v err=%v", done, err)
	}
}
