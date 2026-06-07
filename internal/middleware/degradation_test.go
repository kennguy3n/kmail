package middleware

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newDegradeCache(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestNewDegradationNilDisabled(t *testing.T) {
	if d := NewDegradation(DegradationConfig{}); d != nil {
		t.Error("nil Cache/HealthCheck should return nil")
	}
	// A nil *Degradation.Wrap passes the handler through unchanged.
	var d *Degradation
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	rec := httptest.NewRecorder()
	d.Wrap(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jmap", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("nil Wrap=%d want pass-through 418", rec.Code)
	}
}

func TestDegradationHealthyCachesAndServes(t *testing.T) {
	cache := newDegradeCache(t)
	healthy := true
	d := NewDegradation(DegradationConfig{
		Cache:       cache,
		HealthCheck: func(context.Context) bool { return healthy },
		ReadPaths:   []string{"/jmap"},
	})
	calls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := d.Wrap(next)

	// Non-read path is passed through untouched.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("non-read path code=%d calls=%d", rec.Code, calls)
	}

	// Healthy read path: served live AND cached.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jmap/get", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("healthy read code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Flip unhealthy: GET is served from the cache with the degraded header.
	healthy = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jmap/get", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("X-KMail-Degraded") != "true" {
		t.Fatalf("degraded read code=%d header=%q body=%s", rec.Code, rec.Header().Get("X-KMail-Degraded"), rec.Body.String())
	}
}

func TestDegradationUnhealthyWriteAnd503(t *testing.T) {
	cache := newDegradeCache(t)
	d := NewDegradation(DegradationConfig{
		Cache:       cache,
		HealthCheck: func(context.Context) bool { return false },
		ReadPaths:   []string{"/jmap"},
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := d.Wrap(next)

	// Unhealthy write path → 503.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jmap/set", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unhealthy write=%d want 503", rec.Code)
	}

	// Unhealthy read with no cached value → 503 (cache miss).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jmap/missing", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unhealthy read miss=%d want 503", rec.Code)
	}
}

// TestDegradationCacheIsTenantScoped guards against cross-tenant /
// cross-user leakage: tenant A warms the cache, then tenant B hits
// the same path during an outage and must NOT receive A's snapshot
// (the cache key is scoped by the auth-populated identity).
func TestDegradationCacheIsTenantScoped(t *testing.T) {
	cache := newDegradeCache(t)
	healthy := true
	d := NewDegradation(DegradationConfig{
		Cache:       cache,
		HealthCheck: func(context.Context) bool { return healthy },
		ReadPaths:   []string{"/jmap/session"},
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tenant":"` + TenantIDFrom(r.Context()) + `"}`))
	})
	h := d.Wrap(next)

	reqAs := func(tenant string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/jmap/session", nil)
		ctx := context.WithValue(r.Context(), ctxKeyTenantID, tenant)
		ctx = context.WithValue(ctx, ctxKeyKChatUserID, "user-"+tenant)
		return r.WithContext(ctx)
	}

	// Warm tenant A's snapshot while healthy.
	recA := httptest.NewRecorder()
	h.ServeHTTP(recA, reqAs("A"))
	if recA.Body.String() != `{"tenant":"A"}` {
		t.Fatalf("warm A body=%s", recA.Body.String())
	}

	// Outage: tenant B hits the same path. B has no cached snapshot,
	// so it must 503 — never A's data.
	healthy = false
	recB := httptest.NewRecorder()
	h.ServeHTTP(recB, reqAs("B"))
	if recB.Code != http.StatusServiceUnavailable {
		t.Fatalf("tenant B got code=%d body=%s, want 503 (no cross-tenant serve)", recB.Code, recB.Body.String())
	}

	// Tenant A still gets its own snapshot during the outage.
	recA2 := httptest.NewRecorder()
	h.ServeHTTP(recA2, reqAs("A"))
	if recA2.Code != http.StatusOK || recA2.Body.String() != `{"tenant":"A"}` || recA2.Header().Get("X-KMail-Degraded") != "true" {
		t.Fatalf("tenant A degraded read code=%d body=%s degraded=%q", recA2.Code, recA2.Body.String(), recA2.Header().Get("X-KMail-Degraded"))
	}
}

// TestDegradationSkipsOversizedBody verifies the MaxCacheBytes
// guard: a response larger than the cap is streamed to the client
// in full but never cached, so a later outage read degrades to 503
// rather than serving a truncated snapshot.
func TestDegradationSkipsOversizedBody(t *testing.T) {
	cache := newDegradeCache(t)
	healthy := true
	big := bytes.Repeat([]byte("x"), 2048)
	d := NewDegradation(DegradationConfig{
		Cache:         cache,
		HealthCheck:   func(context.Context) bool { return healthy },
		ReadPaths:     []string{"/jmap/session"},
		MaxCacheBytes: 1024,
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(big)
	})
	h := d.Wrap(next)

	// Healthy: client gets the full body even though it exceeds the cap.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jmap/session", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != len(big) {
		t.Fatalf("healthy oversized read code=%d len=%d want 200/%d", rec.Code, rec.Body.Len(), len(big))
	}

	// Outage: nothing was cached (over the cap) → 503, not a partial body.
	healthy = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jmap/session", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("oversized response was cached: code=%d len=%d want 503", rec.Code, rec.Body.Len())
	}
}
