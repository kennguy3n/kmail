package smartfeatures

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
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

func TestContactTracker_RecordAndTop(t *testing.T) {
	ctx := context.Background()
	tr, err := NewContactTracker(newTestRedis(t), time.Hour)
	if err != nil {
		t.Fatalf("NewContactTracker: %v", err)
	}

	// alice emailed 3x, bob 1x.
	for i := 0; i < 3; i++ {
		if err := tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com"}); err != nil {
			t.Fatalf("RecordSend: %v", err)
		}
	}
	if err := tr.RecordSend(ctx, "t1", "u1", []string{"bob@example.com"}); err != nil {
		t.Fatalf("RecordSend: %v", err)
	}

	top, err := tr.TopContacts(ctx, "t1", "u1", 10)
	if err != nil {
		t.Fatalf("TopContacts: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("expected 2 contacts, got %d", len(top))
	}
	if top[0].Email != "alice@example.com" || top[0].Count != 3 {
		t.Fatalf("top contact wrong: %#v", top[0])
	}
}

func TestContactTracker_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com"})

	other, err := tr.TopContacts(ctx, "t2", "u1", 10)
	if err != nil {
		t.Fatalf("TopContacts: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("expected tenant isolation, got %#v", other)
	}
}

func TestContactTracker_SendCount(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "alice@example.com"})

	// Same address listed twice in one send counts once.
	c, err := tr.SendCount(ctx, "t1", "u1", "ALICE@example.com")
	if err != nil {
		t.Fatalf("SendCount: %v", err)
	}
	if c != 1 {
		t.Fatalf("SendCount = %v, want 1", c)
	}
	miss, err := tr.SendCount(ctx, "t1", "u1", "nobody@example.com")
	if err != nil {
		t.Fatalf("SendCount miss: %v", err)
	}
	if miss != 0 {
		t.Fatalf("SendCount miss = %v, want 0", miss)
	}
}

func TestContactTracker_CoRecipients(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	// alice + bob emailed together twice, alice + carol once.
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "bob@example.com"})
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "bob@example.com"})
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "carol@example.com"})

	sug, err := tr.SuggestCoRecipients(ctx, "t1", "u1", "alice@example.com", nil, 5)
	if err != nil {
		t.Fatalf("SuggestCoRecipients: %v", err)
	}
	if len(sug) != 2 {
		t.Fatalf("expected 2 co-recipients, got %#v", sug)
	}
	if sug[0].Email != "bob@example.com" {
		t.Fatalf("expected bob first, got %#v", sug)
	}
	// Excluding bob (already on the draft) drops him.
	sug2, _ := tr.SuggestCoRecipients(ctx, "t1", "u1", "alice@example.com", []string{"bob@example.com"}, 5)
	if len(sug2) != 1 || sug2[0].Email != "carol@example.com" {
		t.Fatalf("exclude failed: %#v", sug2)
	}
}

func TestContactTracker_RecordValidations(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	if err := tr.RecordSend(ctx, "", "u1", []string{"a@b.com"}); err == nil {
		t.Fatalf("expected error for empty tenant")
	}
	// Empty recipients is a no-op, not an error.
	if err := tr.RecordSend(ctx, "t1", "u1", nil); err != nil {
		t.Fatalf("empty recipients should be no-op: %v", err)
	}
}
