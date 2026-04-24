package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeStore records every Incr call it sees and returns a
// deterministic counter per key. Used instead of a real Valkey for
// unit tests.
type fakeStore struct {
	counts map[string]int64
	fail   error
}

func (f *fakeStore) IncrWithTTL(_ context.Context, key string, _ time.Duration) (int64, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	if f.counts == nil {
		f.counts = map[string]int64{}
	}
	f.counts[key]++
	return f.counts[key], nil
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
