package tenant

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestShardHealthCheckDB drives the HTTP health probe loop: a 2xx
// shard is marked active, a 5xx shard is marked offline, and a
// draining shard is left untouched.
func TestShardHealthCheckDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewShardService(pool, log.New(io.Discard, "", 0))
	// Short timeout so probing any pre-existing junk shards (with
	// unresolvable URLs) fails fast rather than blocking the test.
	svc.HTTPClient = &http.Client{Timeout: 800 * time.Millisecond}
	ctx := context.Background()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sick.Close()

	shHealthy, err := svc.RegisterShard(ctx, Shard{Name: uniqueName("hc-ok"), StalwartURL: healthy.URL, MaxMailboxes: 10})
	if err != nil {
		t.Fatalf("register healthy: %v", err)
	}
	shSick, err := svc.RegisterShard(ctx, Shard{Name: uniqueName("hc-sick"), StalwartURL: sick.URL, MaxMailboxes: 10})
	if err != nil {
		t.Fatalf("register sick: %v", err)
	}
	shDraining, err := svc.RegisterShard(ctx, Shard{Name: uniqueName("hc-drain"), StalwartURL: healthy.URL, MaxMailboxes: 10})
	if err != nil {
		t.Fatalf("register draining: %v", err)
	}
	if _, err := svc.UpdateShard(ctx, shDraining.ID, Shard{Status: ShardStatusDraining}); err != nil {
		t.Fatalf("set draining: %v", err)
	}
	t.Cleanup(func() {
		for _, id := range []string{shHealthy.ID, shSick.ID, shDraining.ID} {
			_, _ = pool.Exec(context.Background(), `DELETE FROM stalwart_shards WHERE id = $1::uuid`, id)
		}
	})

	if err := svc.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}

	gotHealthy, _ := svc.GetShard(ctx, shHealthy.ID)
	if gotHealthy.Status != ShardStatusActive || gotHealthy.HealthCheckedAt.IsZero() {
		t.Errorf("healthy shard=%+v want active+stamped", gotHealthy)
	}
	gotSick, _ := svc.GetShard(ctx, shSick.ID)
	if gotSick.Status != ShardStatusOffline {
		t.Errorf("sick shard status=%q want offline", gotSick.Status)
	}
	gotDrain, _ := svc.GetShard(ctx, shDraining.ID)
	if gotDrain.Status != ShardStatusDraining {
		t.Errorf("draining shard status=%q want draining (should be skipped)", gotDrain.Status)
	}
}

// TestHealthWorkerRun verifies the worker no-ops with a nil service
// and exits promptly on context cancellation after its first tick.
func TestHealthWorkerRun(t *testing.T) {
	// nil service returns immediately.
	(&HealthWorker{}).Run(context.Background())

	pool := testsupport.Pool(t)
	svc := NewShardService(pool, log.New(io.Discard, "", 0))
	svc.HTTPClient = &http.Client{Timeout: 500 * time.Millisecond}
	w := &HealthWorker{Service: svc, Interval: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HealthWorker.Run did not return after context cancel")
	}
}

// TestGetSecondaryShardsDB exercises the failover lookup: a backup
// shard wired via shard_failover_config is returned for the tenant's
// primary shard.
func TestGetSecondaryShardsDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewShardService(pool, log.New(io.Discard, "", 0))
	ctx := context.Background()
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")

	primary, err := svc.RegisterShard(ctx, Shard{Name: uniqueName("pri"), StalwartURL: "http://pri:8080", MaxMailboxes: 100})
	if err != nil {
		t.Fatalf("register primary: %v", err)
	}
	backup, err := svc.RegisterShard(ctx, Shard{Name: uniqueName("bak"), StalwartURL: "http://bak:8080", MaxMailboxes: 100})
	if err != nil {
		t.Fatalf("register backup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenant_shard_assignments WHERE tenant_id = $1::uuid`, tenant)
		for _, id := range []string{primary.ID, backup.ID} {
			_, _ = pool.Exec(context.Background(), `DELETE FROM stalwart_shards WHERE id = $1::uuid`, id)
		}
	})

	// Pin the tenant onto the primary shard.
	if _, err := pool.Exec(ctx, `
		INSERT INTO tenant_shard_assignments (tenant_id, shard_id)
		VALUES ($1::uuid, $2::uuid)
		ON CONFLICT (tenant_id) DO UPDATE SET shard_id = EXCLUDED.shard_id
	`, tenant, primary.ID); err != nil {
		t.Fatalf("assign primary: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO shard_failover_config (shard_id, backup_shard_id, priority)
		VALUES ($1::uuid, $2::uuid, 1)
	`, primary.ID, backup.ID); err != nil {
		t.Fatalf("failover config: %v", err)
	}

	secondaries, err := svc.GetSecondaryShards(ctx, tenant)
	if err != nil {
		t.Fatalf("GetSecondaryShards: %v", err)
	}
	if len(secondaries) != 1 || secondaries[0] != "http://bak:8080" {
		t.Errorf("secondaries=%v want [http://bak:8080]", secondaries)
	}

	// Guard clauses.
	if _, err := svc.GetSecondaryShards(ctx, ""); err == nil {
		t.Error("empty tenantID should error")
	}
	if got, _ := NewShardService(nil, nil).GetSecondaryShards(ctx, tenant); got != nil {
		t.Errorf("nil pool should return nil, got %v", got)
	}
}
