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
	svc.cache["tenant-1"] = "http://cached-shard"
	got, err := svc.GetTenantShard(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("cache hit failed: %v", err)
	}
	if got != "http://cached-shard" {
		t.Errorf("got %q, want http://cached-shard", got)
	}
	svc.invalidate("tenant-1")
	if _, ok := svc.cache["tenant-1"]; ok {
		t.Errorf("invalidate did not clear cache")
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
