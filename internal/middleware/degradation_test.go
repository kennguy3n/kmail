package middleware

import (
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
