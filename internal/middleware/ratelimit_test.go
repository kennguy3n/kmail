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
// trim-then-admit logic the production Lua script does, so the
// tests are exercising the full sliding-window semantics rather
// than a stub that always says yes.
type fakeStore struct {
	log  map[string][]time.Time
	fail error
}

func (f *fakeStore) Allow(_ context.Context, key string, window time.Duration, limit int, now time.Time) (int64, bool, error) {
	if f.fail != nil {
		return 0, false, f.fail
	}
	if f.log == nil {
		f.log = map[string][]time.Time{}
	}
	cutoff := now.Add(-window)
	kept := f.log[key][:0]
	for _, ts := range f.log[key] {
		if ts.After(cutoff) || ts.Equal(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= limit {
		f.log[key] = kept
		return int64(len(kept)), false, nil
	}
	kept = append(kept, now)
	f.log[key] = kept
	return int64(len(kept)), true, nil
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
