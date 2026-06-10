package middleware

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
)

// fakeProvisioner records EnsureTenant calls and optionally fails.
type fakeProvisioner struct {
	calls atomic.Int32
	err   error
	seen  []string
}

func (f *fakeProvisioner) EnsureTenant(_ context.Context, tenantID string) error {
	f.calls.Add(1)
	f.seen = append(f.seen, tenantID)
	return f.err
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func okHandler(served *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Store(true)
		w.WriteHeader(http.StatusOK)
	})
}

func TestLazyProvision_PanicsWithoutProvisioner(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when Provisioner is nil")
		}
	}()
	NewLazyProvision(LazyProvisionConfig{})
}

func TestLazyProvision_NoTenantIsPassThrough(t *testing.T) {
	fp := &fakeProvisioner{}
	lp := NewLazyProvision(LazyProvisionConfig{Provisioner: fp, Logger: discardLogger()})
	var served atomic.Bool

	req := httptest.NewRequest(http.MethodGet, "/", nil) // no tenant in context
	w := httptest.NewRecorder()
	lp.Wrap(okHandler(&served)).ServeHTTP(w, req)

	if !served.Load() {
		t.Fatal("request should be served")
	}
	if fp.calls.Load() != 0 {
		t.Errorf("EnsureTenant called %d times, want 0 (no tenant)", fp.calls.Load())
	}
}

func TestLazyProvision_ProvisionsAuthenticatedTenant(t *testing.T) {
	fp := &fakeProvisioner{}
	lp := NewLazyProvision(LazyProvisionConfig{Provisioner: fp, Logger: discardLogger()})
	var served atomic.Bool

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(WithTenantID(req.Context(), "tenant-9"))
	w := httptest.NewRecorder()
	lp.Wrap(okHandler(&served)).ServeHTTP(w, req)

	if !served.Load() {
		t.Fatal("request should be served")
	}
	if fp.calls.Load() != 1 {
		t.Fatalf("EnsureTenant called %d times, want 1", fp.calls.Load())
	}
	if len(fp.seen) != 1 || fp.seen[0] != "tenant-9" {
		t.Errorf("provisioned %v, want [tenant-9]", fp.seen)
	}
}

// Fails OPEN: a provisioning error must not turn the request into a
// 500; the handler still runs.
func TestLazyProvision_FailsOpen(t *testing.T) {
	fp := &fakeProvisioner{err: errors.New("db down")}
	lp := NewLazyProvision(LazyProvisionConfig{Provisioner: fp, Logger: discardLogger()})
	var served atomic.Bool

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(WithTenantID(req.Context(), "tenant-9"))
	w := httptest.NewRecorder()
	lp.Wrap(okHandler(&served)).ServeHTTP(w, req)

	if w.Code != http.StatusOK || !served.Load() {
		t.Fatalf("status = %d, served = %v; want request to proceed despite provision error", w.Code, served.Load())
	}
}

// With no cache, EnsureTenant is invoked on every request (its own
// SELECT fast-path keeps that cheap). This documents the nil-cache
// contract relied on by deployments without Valkey.
func TestLazyProvision_NoCacheCallsEveryRequest(t *testing.T) {
	fp := &fakeProvisioner{}
	lp := NewLazyProvision(LazyProvisionConfig{Provisioner: fp, Logger: discardLogger()})
	h := lp.Wrap(okHandler(&atomic.Bool{}))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(WithTenantID(req.Context(), "tenant-9"))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if fp.calls.Load() != 3 {
		t.Errorf("EnsureTenant called %d times, want 3 (no cache)", fp.calls.Load())
	}
}

// isCachedProvisioned must treat a nil cache as a miss (so
// provisioning still runs) rather than panicking.
func TestLazyProvision_NilCacheIsMiss(t *testing.T) {
	lp := NewLazyProvision(LazyProvisionConfig{Provisioner: &fakeProvisioner{}, Logger: discardLogger()})
	if lp.isCachedProvisioned(context.Background(), "t1") {
		t.Fatal("nil cache should report a miss")
	}
	// markCachedProvisioned must be a safe no-op with a nil cache.
	lp.markCachedProvisioned(context.Background(), "t1")
}

// Guards against a misconfigured ttl: a zero/negative CacheTTL must
// fall back to the default rather than producing a non-expiring key.
func TestLazyProvision_DefaultTTL(t *testing.T) {
	lp := NewLazyProvision(LazyProvisionConfig{Provisioner: &fakeProvisioner{}})
	if lp.ttl != defaultProvisionCacheTTL {
		t.Errorf("ttl = %v, want default %v", lp.ttl, defaultProvisionCacheTTL)
	}
	// Compile-time assurance the cache field is the expected type.
	var _ *redis.Client = lp.cache
}
