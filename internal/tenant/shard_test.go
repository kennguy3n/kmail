package tenant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestShardServiceCache(t *testing.T) {
	t.Parallel()
	svc := NewShardService(nil, nil)
	svc.cache.Add("tenant-1", "http://cached-shard")
	got, err := svc.GetTenantShard(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("cache hit failed: %v", err)
	}
	if got != "http://cached-shard" {
		t.Errorf("got %q, want http://cached-shard", got)
	}
	svc.invalidate("tenant-1")
	if _, ok := svc.cache.Get("tenant-1"); ok {
		t.Errorf("invalidate did not clear cache")
	}
}

// TestShardServiceIDCacheHit verifies GetTenantShardID consults
// the dedicated idCache before falling through to Postgres. The
// shared search backends call this on every indexed message and
// every search; a DB round-trip per call dominates latency at
// shard-pooled scale.
func TestShardServiceIDCacheHit(t *testing.T) {
	t.Parallel()
	svc := NewShardService(nil, nil)
	svc.idCache.Add("tenant-1", "shard-abc")
	// Pool is nil — if the cache miss path runs we get
	// ErrNoCapacity, so a successful return value is positive
	// evidence the cache served the call.
	got, err := svc.GetTenantShardID(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("idCache hit failed: %v", err)
	}
	if got != "shard-abc" {
		t.Errorf("got %q, want shard-abc", got)
	}
}

// TestShardServiceIDCacheInvalidate confirms `invalidate` evicts
// from BOTH caches in lockstep so a Rebalance / Assign write
// doesn't leave a stale shard id pinned on the URL cache (or
// vice-versa).
func TestShardServiceIDCacheInvalidate(t *testing.T) {
	t.Parallel()
	svc := NewShardService(nil, nil)
	svc.cache.Add("tenant-1", "http://cached")
	svc.idCache.Add("tenant-1", "shard-abc")
	svc.invalidate("tenant-1")
	if _, ok := svc.cache.Get("tenant-1"); ok {
		t.Errorf("invalidate left URL cache populated")
	}
	if _, ok := svc.idCache.Get("tenant-1"); ok {
		t.Errorf("invalidate left ID cache populated")
	}
}

// TestShardServiceGetTenantShardIDEmpty confirms the empty-tenant
// validation guard matches the URL variant — a misconfigured
// caller should fail fast, not return an empty shard id.
func TestShardServiceGetTenantShardIDEmpty(t *testing.T) {
	t.Parallel()
	svc := NewShardService(nil, nil)
	if _, err := svc.GetTenantShardID(context.Background(), ""); err == nil {
		t.Error("expected error for empty tenant")
	}
}

func TestShardProbe(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.Error(w, "not found", 404)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := NewShardService(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !svc.probe(ctx, srv.URL) {
		t.Errorf("probe should succeed for live server")
	}
	if svc.probe(ctx, "http://127.0.0.1:1") {
		t.Errorf("probe should fail for dead server")
	}
}

func TestShardServiceGetTenantShardNoPool(t *testing.T) {
	t.Parallel()
	svc := NewShardService(nil, nil)
	if _, err := svc.GetTenantShard(context.Background(), ""); err == nil {
		t.Error("expected error for empty tenant")
	}
}
