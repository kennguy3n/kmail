package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func bearerReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestSessionIDFromRequest_StableAndScoped(t *testing.T) {
	a := SessionIDFromRequest(bearerReq("token-aaa"))
	b := SessionIDFromRequest(bearerReq("token-aaa"))
	c := SessionIDFromRequest(bearerReq("token-bbb"))
	if a == "" || a != b {
		t.Fatalf("same token must yield same id: %q vs %q", a, b)
	}
	if a == c {
		t.Fatal("different tokens must yield different ids")
	}
	if len(a) != 32 {
		t.Fatalf("expected 32-char id, got %d", len(a))
	}
	if got := SessionIDFromRequest(httptest.NewRequest(http.MethodGet, "/", nil)); got != "" {
		t.Fatalf("no bearer token must yield empty id, got %q", got)
	}
}

func TestMemoryStore_TouchEvictsOldestOverCap(t *testing.T) {
	ctx := context.Background()
	st := NewMemorySessionStore()
	base := time.Unix(1_700_000_000, 0)
	uk := userKey("t1", "u1")

	// 3 sessions, cap 3 — no eviction.
	for i := 0; i < 3; i++ {
		ev, err := st.Touch(ctx, SessionInfo{ID: idFor(i), UserKey: uk}, time.Hour, 3, base.Add(time.Duration(i)*time.Minute))
		if err != nil || len(ev) != 0 {
			t.Fatalf("unexpected eviction at i=%d: %v %v", i, ev, err)
		}
	}
	// 4th session over cap 3 → oldest (id-0) evicted.
	ev, err := st.Touch(ctx, SessionInfo{ID: idFor(3), UserKey: uk}, time.Hour, 3, base.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0] != idFor(0) {
		t.Fatalf("expected oldest id-0 evicted, got %v", ev)
	}
	live, _ := st.List(ctx, uk, time.Hour, base.Add(3*time.Minute))
	if len(live) != 3 {
		t.Fatalf("expected 3 live, got %d", len(live))
	}
	// Newest first.
	if live[0].ID != idFor(3) {
		t.Fatalf("expected newest first, got %s", live[0].ID)
	}
}

func TestMemoryStore_TouchRefreshesWithoutDuplicating(t *testing.T) {
	ctx := context.Background()
	st := NewMemorySessionStore()
	base := time.Unix(1_700_000_000, 0)
	uk := userKey("t1", "u1")
	_, _ = st.Touch(ctx, SessionInfo{ID: "s", UserKey: uk, CreatedAt: base}, time.Hour, 5, base)
	// Touch again later — must not create a second entry, must keep CreatedAt.
	_, _ = st.Touch(ctx, SessionInfo{ID: "s", UserKey: uk, CreatedAt: base.Add(time.Hour)}, time.Hour, 5, base.Add(30*time.Minute))
	live, _ := st.List(ctx, uk, time.Hour, base.Add(30*time.Minute))
	if len(live) != 1 {
		t.Fatalf("expected 1 session, got %d", len(live))
	}
	if !live[0].CreatedAt.Equal(base) {
		t.Fatalf("CreatedAt must be preserved across touch, got %v", live[0].CreatedAt)
	}
}

func TestMemoryStore_IdleTimeoutPrunes(t *testing.T) {
	ctx := context.Background()
	st := NewMemorySessionStore()
	base := time.Unix(1_700_000_000, 0)
	uk := userKey("t1", "u1")
	_, _ = st.Touch(ctx, SessionInfo{ID: "s", UserKey: uk}, time.Hour, 5, base)
	// 2h later with 1h idle window → pruned.
	live, _ := st.List(ctx, uk, time.Hour, base.Add(2*time.Hour))
	if len(live) != 0 {
		t.Fatalf("expected idle session pruned, got %d", len(live))
	}
}

func TestMemoryStore_RevokeAndIsRevoked(t *testing.T) {
	ctx := context.Background()
	st := NewMemorySessionStore()
	now := time.Unix(1_700_000_000, 0)
	uk := userKey("t1", "u1")
	_, _ = st.Touch(ctx, SessionInfo{ID: "s", UserKey: uk}, time.Hour, 5, now)

	if r, _ := st.IsRevoked(ctx, "s", now); r {
		t.Fatal("not revoked yet")
	}
	_ = st.Revoke(ctx, uk, "s", time.Hour, now)
	if r, _ := st.IsRevoked(ctx, "s", now); !r {
		t.Fatal("should be revoked")
	}
	// Removed from live set.
	if live, _ := st.List(ctx, uk, time.Hour, now); len(live) != 0 {
		t.Fatalf("revoked session must leave live set, got %d", len(live))
	}
	// Tombstone expires.
	if r, _ := st.IsRevoked(ctx, "s", now.Add(2*time.Hour)); r {
		t.Fatal("tombstone should have expired")
	}
}

func idFor(i int) string { return "id-" + string(rune('0'+i)) }

// --- Manager middleware ---

// ctxWithIdentity injects tenant/user the way the OIDC middleware
// would, so the session middleware can read them.
func ctxWithIdentity(r *http.Request, tenant, user string) *http.Request {
	ctx := context.WithValue(r.Context(), ctxKeyTenantID, tenant)
	ctx = context.WithValue(ctx, ctxKeyKChatUserID, user)
	return r.WithContext(ctx)
}

func TestSessionManager_DisabledIsPassthrough(t *testing.T) {
	mgr := NewSessionManager(SessionConfig{Store: NewMemorySessionStore(), Enabled: false})
	called := false
	h := mgr.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := ctxWithIdentity(bearerReq("tok"), "t1", "u1")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("disabled manager must pass through")
	}
}

func TestSessionManager_RevokedTokenRejected(t *testing.T) {
	st := NewMemorySessionStore()
	mgr := NewSessionManager(SessionConfig{Store: st, Enabled: true})
	r := ctxWithIdentity(bearerReq("tok"), "t1", "u1")
	sid := SessionIDFromRequest(r)
	_ = st.Revoke(context.Background(), userKey("t1", "u1"), sid, time.Hour, time.Now())

	rec := httptest.NewRecorder()
	mgr.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run for revoked session")
	})).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestSessionManager_TracksSession(t *testing.T) {
	st := NewMemorySessionStore()
	mgr := NewSessionManager(SessionConfig{Store: st, Enabled: true})
	r := ctxWithIdentity(bearerReq("tok"), "t1", "u1")
	mgr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), r)

	live, _ := st.List(context.Background(), userKey("t1", "u1"), DefaultSessionIdleTimeout, time.Now())
	if len(live) != 1 {
		t.Fatalf("expected session tracked, got %d", len(live))
	}
}

func TestSessionManager_NoBearerPassesThrough(t *testing.T) {
	st := NewMemorySessionStore()
	mgr := NewSessionManager(SessionConfig{Store: st, Enabled: true})
	called := false
	r := ctxWithIdentity(httptest.NewRequest(http.MethodGet, "/", nil), "t1", "u1") // no bearer
	mgr.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("request without bearer token must pass through (e.g. dev header auth)")
	}
}

// --- Handlers ---

func TestSessionHandlers_ListAndRevoke(t *testing.T) {
	st := NewMemorySessionStore()
	mgr := NewSessionManager(SessionConfig{Store: st, Enabled: true})
	h := NewSessionHandlers(mgr)
	uk := userKey("t1", "u1")
	now := time.Now()

	// Seed two sessions for the user.
	curReq := ctxWithIdentity(bearerReq("current"), "t1", "u1")
	curSID := SessionIDFromRequest(curReq)
	otherSID := SessionIDFromRequest(bearerReq("other"))
	_, _ = st.Touch(context.Background(), SessionInfo{ID: otherSID, UserKey: uk}, DefaultSessionIdleTimeout, 5, now.Add(-time.Minute))
	_, _ = st.Touch(context.Background(), SessionInfo{ID: curSID, UserKey: uk}, DefaultSessionIdleTimeout, 5, now)

	// List marks the current session.
	rec := httptest.NewRecorder()
	h.list(rec, curReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d", rec.Code)
	}
	var listResp sessionListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(listResp.Sessions))
	}
	var sawCurrent bool
	for _, s := range listResp.Sessions {
		if s.ID == curSID && s.Current {
			sawCurrent = true
		}
	}
	if !sawCurrent {
		t.Fatal("current session must be flagged")
	}

	// Revoke all others: leaves the current session live, revokes the other.
	revReq := ctxWithIdentity(
		httptest.NewRequest(http.MethodPost, "/api/v1/sessions/revoke",
			strings.NewReader(`{"all_others":true}`)), "t1", "u1")
	revReq.Header.Set("Authorization", "Bearer current")
	rec = httptest.NewRecorder()
	h.revoke(rec, revReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status %d body=%s", rec.Code, rec.Body.String())
	}
	if r, _ := st.IsRevoked(context.Background(), otherSID, time.Now()); !r {
		t.Fatal("other session should be revoked")
	}
	if r, _ := st.IsRevoked(context.Background(), curSID, time.Now()); r {
		t.Fatal("current session must NOT be revoked by all_others")
	}
}

func TestSessionHandlers_RevokeRequiresArg(t *testing.T) {
	st := NewMemorySessionStore()
	mgr := NewSessionManager(SessionConfig{Store: st, Enabled: true})
	h := NewSessionHandlers(mgr)
	req := ctxWithIdentity(
		httptest.NewRequest(http.MethodPost, "/api/v1/sessions/revoke", strings.NewReader(`{}`)),
		"t1", "u1")
	rec := httptest.NewRecorder()
	h.revoke(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSessionHandlers_NoStore503(t *testing.T) {
	mgr := NewSessionManager(SessionConfig{Enabled: true}) // nil store
	h := NewSessionHandlers(mgr)
	rec := httptest.NewRecorder()
	h.list(rec, ctxWithIdentity(bearerReq("t"), "t1", "u1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}
