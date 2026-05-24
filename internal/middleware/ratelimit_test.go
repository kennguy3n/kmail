package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeStore implements RateLimiterStore in-process so the
// middleware tests don't need Valkey. Each `Allow` call records the
// request timestamp in a per-key slice and applies the same
// trim-then-admit-then-rollback logic the production Lua script
// does, so the tests are exercising the full sliding-window
// semantics — including the user-rejection-rolls-back-tenant
// invariant — rather than a stub that always says yes.
type fakeStore struct {
	log  map[string][]time.Time
	fail error
}

// trimAndCount returns the surviving entries for `key` after
// dropping anything older than `now - window`. Mutates f.log[key]
// in place to keep it bounded.
func (f *fakeStore) trimAndCount(key string, window time.Duration, now time.Time) []time.Time {
	cutoff := now.Add(-window)
	kept := f.log[key][:0]
	for _, ts := range f.log[key] {
		if ts.After(cutoff) || ts.Equal(cutoff) {
			kept = append(kept, ts)
		}
	}
	f.log[key] = kept
	return kept
}

func (f *fakeStore) Allow(_ context.Context, tenantKey, userKey string, window time.Duration, tenantLimit, userLimit int, now time.Time) (bool, bool, error) {
	if f.fail != nil {
		return false, false, f.fail
	}
	if f.log == nil {
		f.log = map[string][]time.Time{}
	}

	tenantKept := f.trimAndCount(tenantKey, window, now)
	if len(tenantKept) >= tenantLimit {
		return false, false, nil
	}
	f.log[tenantKey] = append(tenantKept, now)

	// No user scope active — caller passed tenantKey as the
	// KEYS[2] placeholder. Mirror the production script and
	// ignore the placeholder.
	if userLimit <= 0 {
		return true, true, nil
	}
	userKept := f.trimAndCount(userKey, window, now)
	if len(userKept) >= userLimit {
		// Roll back the tenant admission so a user-level
		// rejection does NOT consume tenant budget. Mirrors
		// the ZREM in the production Lua script. The return
		// `(true, false)` reports "tenant check passed, user
		// check failed" so the middleware attributes the
		// rejection to the user scope.
		tenantLog := f.log[tenantKey]
		f.log[tenantKey] = tenantLog[:len(tenantLog)-1]
		return true, false, nil
	}
	f.log[userKey] = append(userKept, now)
	return true, true, nil
}

// IncrWithTTL satisfies the RateLimiterStore interface for the
// bucket-counter primitive (used by downstream consumers like
// the integrations dispatcher). The middleware-level RateLimiter
// tests in this file do NOT exercise IncrWithTTL — those live
// against the integrations package's own fake. We keep a simple
// in-memory counter here so tests that wire the same fake into
// both Allow and IncrWithTTL call sites still observe meaningful
// behaviour, and honour f.fail so failure injection works
// uniformly across both surfaces.
func (f *fakeStore) IncrWithTTL(_ context.Context, key string, _ time.Duration) (int64, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	if f.log == nil {
		f.log = map[string][]time.Time{}
	}
	// Reuse f.log as a side-effect-free count: number of
	// entries == number of increments. Use a zero-time
	// placeholder since we don't need the timestamp.
	f.log[key] = append(f.log[key], time.Time{})
	return int64(len(f.log[key])), nil
}

// authedRequest returns an httptest request with tenant + user
// context applied so the limiter can extract the identity.
func authedRequest(tenant, user string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxKeyTenantID, tenant)
	ctx = context.WithValue(ctx, ctxKeyKChatUserID, user)
	return req.WithContext(ctx)
}

func TestRateLimiter_AllowsBelowCeiling(t *testing.T) {
	store := &fakeStore{}
	rl := NewRateLimiter(RateLimiterConfig{
		Client:    store,
		TenantRPM: 10,
		UserRPM:   5,
		Window:    time.Minute,
		Now:       func() time.Time { return time.Unix(0, 0) },
	})
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedRequest("t1", "u1"))
		if rec.Code != http.StatusOK {
			t.Errorf("iter %d: expected 200, got %d", i, rec.Code)
		}
	}
}

func TestRateLimiter_RejectsOnUserCeiling(t *testing.T) {
	store := &fakeStore{}
	rl := NewRateLimiter(RateLimiterConfig{
		Client:    store,
		TenantRPM: 1000,
		UserRPM:   3,
		Window:    time.Minute,
		Now:       func() time.Time { return time.Unix(0, 0) },
	})
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First 3 should pass, 4th should be blocked.
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedRequest("t1", "u1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d: expected 200, got %d", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header")
	}
	if rec.Header().Get("X-RateLimit-Scope") != "user" {
		t.Errorf("expected user scope, got %q", rec.Header().Get("X-RateLimit-Scope"))
	}
}

func TestRateLimiter_RejectsOnTenantCeiling(t *testing.T) {
	store := &fakeStore{}
	rl := NewRateLimiter(RateLimiterConfig{
		Client:    store,
		TenantRPM: 2,
		UserRPM:   100,
		Window:    time.Minute,
		Now:       func() time.Time { return time.Unix(0, 0) },
	})
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Two distinct users sharing the same tenant — tenant ceiling
	// kicks in before either user ceiling.
	for i, user := range []string{"u1", "u2"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedRequest("t1", user))
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d (%s): expected 200, got %d", i, user, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u3"))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Scope") != "tenant" {
		t.Errorf("expected tenant scope, got %q", rec.Header().Get("X-RateLimit-Scope"))
	}
}

func TestRateLimiter_FailsOpenOnStoreError(t *testing.T) {
	store := &fakeStore{fail: errors.New("valkey down")}
	rl := NewRateLimiter(RateLimiterConfig{
		Client:    store,
		TenantRPM: 1,
		UserRPM:   1,
		Window:    time.Minute,
		Now:       func() time.Time { return time.Unix(0, 0) },
	})
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u1"))
	if rec.Code != http.StatusOK {
		t.Errorf("expected fail-open, got %d", rec.Code)
	}
}

func TestRateLimiter_NoOp_WhenClientNil(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{Client: nil})
	called := false
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), authedRequest("t1", "u1"))
	if !called {
		t.Error("expected next handler to be invoked when Client is nil")
	}
}

func TestRateLimiter_BucketsByWindow(t *testing.T) {
	store := &fakeStore{}
	now := time.Unix(0, 0)
	rl := NewRateLimiter(RateLimiterConfig{
		Client:    store,
		TenantRPM: 2,
		UserRPM:   2,
		Window:    time.Minute,
		Now:       func() time.Time { return now },
	})
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Fill the first window.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedRequest("t1", "u1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d: expected 200, got %d", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 within same window, got %d", rec.Code)
	}

	// Advance past the window boundary — counter resets.
	now = now.Add(2 * time.Minute)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u1"))
	if rec.Code != http.StatusOK {
		t.Errorf("expected reset after window, got %d", rec.Code)
	}
}

// TestRateLimiter_SlidingWindow_NoBoundaryBurst pins the property
// the migration to a sliding-window log was meant to provide: a
// caller cannot double their effective RPM by timing requests
// around a bucket boundary. With the previous fixed-window
// counter, 2 requests at t=59s + 2 requests at t=61s would all
// succeed (4 admitted within ~2 seconds, 4x the configured RPM=2).
// With the sliding-window log every request inside the rolling
// 60-second window counts, so the second batch is rejected until
// the first ages out.
func TestRateLimiter_SlidingWindow_NoBoundaryBurst(t *testing.T) {
	store := &fakeStore{}
	now := time.Unix(0, 0)
	rl := NewRateLimiter(RateLimiterConfig{
		Client:    store,
		TenantRPM: 1000,
		UserRPM:   2,
		Window:    time.Minute,
		Now:       func() time.Time { return now },
	})
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// At t=0s: 2 admitted (fills the user window).
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedRequest("t1", "u1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d at t=0: expected 200, got %d", i, rec.Code)
		}
	}

	// At t=59s: still inside the rolling window, must reject.
	now = time.Unix(59, 0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 at t=59s (still inside window), got %d", rec.Code)
	}

	// At t=61s: the two t=0 entries have just rolled out of the
	// window (window=60s, cutoff=t=1s). New request admitted.
	now = time.Unix(61, 0)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 at t=61s (oldest entries aged out), got %d", rec.Code)
	}

	// At t=62s: only 1 admitted in the rolling window so far
	// (the t=61s one), so room for 1 more.
	now = time.Unix(62, 0)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 at t=62s (one slot still free), got %d", rec.Code)
	}

	// At t=63s: the two t=61s/t=62s entries now fill the
	// rolling window. Must reject — this is the property the
	// old fixed-window counter violated.
	now = time.Unix(63, 0)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 at t=63s (window full again), got %d", rec.Code)
	}
}

// TestRateLimiter_UserRejection_DoesNotConsumeTenantBudget pins the
// atomic-rollback invariant: when the tenant check passes but the
// user check fails, the tenant counter must be restored so a
// chatty user can't starve their own tenant's budget. The previous
// two-call implementation violated this — every user-level
// rejection still incremented the tenant counter.
func TestRateLimiter_UserRejection_DoesNotConsumeTenantBudget(t *testing.T) {
	store := &fakeStore{}
	now := time.Unix(0, 0)
	rl := NewRateLimiter(RateLimiterConfig{
		Client:    store,
		TenantRPM: 5, // small enough that we can see budget drift
		UserRPM:   1, // single-shot per user inside the window
		Window:    time.Minute,
		Now:       func() time.Time { return now },
	})
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request from u1: tenant=1, user=1, admitted.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first u1 request: expected 200, got %d", rec.Code)
	}

	// 10 more u1 requests, all rejected at the user ceiling.
	// Each rejection must roll back the tenant admission so the
	// tenant budget is unaffected.
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedRequest("t1", "u1"))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("iter %d u1: expected 429, got %d", i, rec.Code)
		}
		if rec.Header().Get("X-RateLimit-Scope") != "user" {
			t.Errorf("iter %d u1: expected user scope, got %q", i, rec.Header().Get("X-RateLimit-Scope"))
		}
	}

	// Now drive 4 more *distinct* users. Each gets a fresh user
	// budget; the tenant ZSET should currently sit at 1 (u1's
	// admitted request). If the rollback worked, all 4 must be
	// admitted (1 + 4 = 5 == TenantRPM). If rollback was
	// missing, the tenant counter would have already reached
	// 11 (1 + 10 inflated) and these would all be 429.
	for i, user := range []string{"u2", "u3", "u4", "u5"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedRequest("t1", user))
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d %s: expected 200 (tenant budget preserved across u1 rejections), got %d / scope=%q",
				i, user, rec.Code, rec.Header().Get("X-RateLimit-Scope"))
		}
	}

	// The next request from a 6th user should hit the tenant
	// ceiling exactly (TenantRPM=5 reached by u1+u2+u3+u4+u5).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", "u6"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("u6 should hit tenant ceiling, got %d", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Scope") != "tenant" {
		t.Errorf("u6: expected tenant scope, got %q", rec.Header().Get("X-RateLimit-Scope"))
	}
}

// TestRateLimiter_TenantOnly_NoUserContext exercises the
// no-user-scope branch: requests without a KChat user ID must
// still rate-limit at the tenant ceiling. This pins the
// `userLimit <= 0` short-circuit AND the cluster-co-located
// `userKey == tenantKey` placeholder added to address the Devin
// Review CROSSSLOT finding.
func TestRateLimiter_TenantOnly_NoUserContext(t *testing.T) {
	store := &fakeStore{}
	now := time.Unix(0, 0)
	rl := NewRateLimiter(RateLimiterConfig{
		Client:    store,
		TenantRPM: 3,
		UserRPM:   100, // user limit irrelevant — no user context
		Window:    time.Minute,
		Now:       func() time.Time { return now },
	})
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Tenant-scoped request with NO user ID. The placeholder
	// userKey must be ignored; only the tenant scope counts.
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedRequest("t1", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d: expected 200, got %d", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest("t1", ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th tenant-only request: expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Scope") != "tenant" {
		t.Errorf("4th request: expected tenant scope, got %q", rec.Header().Get("X-RateLimit-Scope"))
	}
	// The user-keyed placeholder must NOT have been written to.
	// Only the tenant ZSET should have entries.
	tenantKey := "kmail:rl:tenant:{t1}"
	if got := len(store.log[tenantKey]); got != 3 {
		t.Errorf("tenant ZSET: expected 3 entries, got %d", got)
	}
	// No `kmail:rl:user:...` key should exist for this tenant.
	for k := range store.log {
		if k != tenantKey {
			t.Errorf("unexpected key written in tenant-only mode: %q (len=%d)", k, len(store.log[k]))
		}
	}
}
