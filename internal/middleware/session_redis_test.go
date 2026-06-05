package middleware

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRedisSessionStore(t *testing.T) (*RedisSessionStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisSessionStore(client), mr
}

func TestRedisSessionStore_TouchListRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, _ := newRedisSessionStore(t)
	uk := userKey("t1", "u1")
	now := time.Unix(1_700_000_000, 0)

	_, err := st.Touch(ctx, SessionInfo{ID: "s1", UserKey: uk, TenantID: "t1", UserID: "u1", IP: "1.2.3.4"}, time.Hour, 5, now)
	if err != nil {
		t.Fatal(err)
	}
	live, err := st.List(ctx, uk, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != "s1" || live[0].IP != "1.2.3.4" {
		t.Fatalf("unexpected list: %+v", live)
	}
	if live[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be stamped by the store")
	}
}

func TestRedisSessionStore_PreservesCreatedAtAcrossTouch(t *testing.T) {
	ctx := context.Background()
	st, _ := newRedisSessionStore(t)
	uk := userKey("t1", "u1")
	t0 := time.Unix(1_700_000_000, 0)
	_, _ = st.Touch(ctx, SessionInfo{ID: "s1", UserKey: uk}, time.Hour, 5, t0)
	_, _ = st.Touch(ctx, SessionInfo{ID: "s1", UserKey: uk}, time.Hour, 5, t0.Add(10*time.Minute))

	live, _ := st.List(ctx, uk, time.Hour, t0.Add(10*time.Minute))
	if len(live) != 1 {
		t.Fatalf("expected 1 session, got %d", len(live))
	}
	if !live[0].CreatedAt.Equal(t0) {
		t.Fatalf("CreatedAt must be preserved, got %v want %v", live[0].CreatedAt, t0)
	}
	if !live[0].LastSeen.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("LastSeen must be refreshed, got %v", live[0].LastSeen)
	}
}

func TestRedisSessionStore_EvictsOldestOverCap(t *testing.T) {
	ctx := context.Background()
	st, _ := newRedisSessionStore(t)
	uk := userKey("t1", "u1")
	base := time.Unix(1_700_000_000, 0)

	for i := 0; i < 3; i++ {
		if _, err := st.Touch(ctx, SessionInfo{ID: idFor(i), UserKey: uk}, time.Hour, 3, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	evicted, err := st.Touch(ctx, SessionInfo{ID: idFor(3), UserKey: uk}, time.Hour, 3, base.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0] != idFor(0) {
		t.Fatalf("expected oldest id-0 evicted, got %v", evicted)
	}
	live, _ := st.List(ctx, uk, time.Hour, base.Add(3*time.Minute))
	if len(live) != 3 {
		t.Fatalf("expected 3 live, got %d", len(live))
	}
}

func TestRedisSessionStore_EnforceCapNeverEvictsCurrentSession(t *testing.T) {
	ctx := context.Background()
	st, _ := newRedisSessionStore(t)
	uk := userKey("t1", "u1")
	base := time.Unix(1_700_000_000, 0)

	// Three sessions under a cap of 3; "cur" is the OLDEST (its
	// CreatedAt is preserved across re-touches).
	cur := "cur-sess"
	if _, err := st.Touch(ctx, SessionInfo{ID: cur, UserKey: uk}, time.Hour, 3, base); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Touch(ctx, SessionInfo{ID: idFor(1), UserKey: uk}, time.Hour, 3, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Touch(ctx, SessionInfo{ID: idFor(2), UserKey: uk}, time.Hour, 3, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Re-touch the oldest (current) session with a lowered cap of 2.
	// Even though cur sorts oldest, the keepID guard must spare it and
	// evict the next-oldest (id-1) instead — without the guard, cur
	// would be evicted and the caller locked out of their own session.
	evicted, err := st.Touch(ctx, SessionInfo{ID: cur, UserKey: uk}, time.Hour, 2, base.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0] != idFor(1) {
		t.Fatalf("expected next-oldest id-1 evicted, got %v", evicted)
	}
	live, _ := st.List(ctx, uk, time.Hour, base.Add(3*time.Minute))
	if len(live) != 2 {
		t.Fatalf("expected 2 live, got %d", len(live))
	}
	found := false
	for _, s := range live {
		if s.ID == cur {
			found = true
		}
	}
	if !found {
		t.Fatal("current session was evicted but must always be retained")
	}
}

func TestRedisSessionStore_RevokeAndIsRevoked(t *testing.T) {
	ctx := context.Background()
	st, _ := newRedisSessionStore(t)
	uk := userKey("t1", "u1")
	now := time.Unix(1_700_000_000, 0)
	_, _ = st.Touch(ctx, SessionInfo{ID: "s1", UserKey: uk}, time.Hour, 5, now)

	if r, _ := st.IsRevoked(ctx, "s1", now); r {
		t.Fatal("not revoked yet")
	}
	if err := st.Revoke(ctx, uk, "s1", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.IsRevoked(ctx, "s1", now); !r {
		t.Fatal("should be revoked")
	}
	if live, _ := st.List(ctx, uk, time.Hour, now); len(live) != 0 {
		t.Fatalf("revoked session must leave live set, got %d", len(live))
	}
}

func TestRedisSessionStore_RevokeForeignSessionRejected(t *testing.T) {
	ctx := context.Background()
	st, _ := newRedisSessionStore(t)
	now := time.Unix(1_700_000_000, 0)
	victim := userKey("t1", "victim")
	attacker := userKey("t1", "attacker")
	_, _ = st.Touch(ctx, SessionInfo{ID: "victim-sess", UserKey: victim}, time.Hour, 5, now)

	// The attacker's set does not contain the victim's session id, so
	// the ownership guard must refuse the revoke and skip the
	// tombstone write entirely.
	if err := st.Revoke(ctx, attacker, "victim-sess", time.Hour, now); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
	if r, _ := st.IsRevoked(ctx, "victim-sess", now); r {
		t.Fatal("foreign revoke must NOT tombstone the victim's session")
	}
	if live, _ := st.List(ctx, victim, time.Hour, now); len(live) != 1 {
		t.Fatalf("victim session must remain live, got %d", len(live))
	}
}

func TestRedisSessionStore_IdleExpiryReapsFromList(t *testing.T) {
	ctx := context.Background()
	st, mr := newRedisSessionStore(t)
	uk := userKey("t1", "u1")
	now := time.Unix(1_700_000_000, 0)
	_, _ = st.Touch(ctx, SessionInfo{ID: "s1", UserKey: uk}, time.Hour, 5, now)

	// Fast-forward miniredis past the idle TTL: the sess: key expires,
	// and List must reconcile it out of the user's set.
	mr.FastForward(2 * time.Hour)
	live, err := st.List(ctx, uk, time.Hour, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("expected expired session reaped, got %d", len(live))
	}
}
