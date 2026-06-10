package billing

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestQuotaWorker_PollTickWritesSnapshotDB exercises the legacy poll
// reconciliation: pollTick scans each active tenant and writes the
// authoritative storage snapshot back to quotas.
func TestQuotaWorker_PollTickWritesSnapshotDB(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")

	svc := NewService(Config{Pool: pool})
	// Seed a quota row so SetStorageUsage has something to update.
	if err := svc.UpsertQuota(ctx, tenant, 1<<30, 1); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}

	scanner := fakeScanner{bytes: map[string]int64{tenant: 4096}}
	w := NewQuotaWorker(QuotaWorkerConfig{
		Pool:    pool,
		Billing: svc,
		Scanner: scanner,
		Mode:    ModePoll,
		Logger:  quietLogger(),
	})
	if w.Mode() != ModePoll {
		t.Fatalf("expected poll mode, got %s", w.Mode())
	}
	if err := w.tick(ctx); err != nil {
		t.Fatalf("poll tick: %v", err)
	}
	q, err := svc.GetQuota(ctx, tenant)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.StorageUsedBytes != 4096 {
		t.Errorf("storage used = %d, want 4096", q.StorageUsedBytes)
	}
}

// TestQuotaWorker_PollTickSkipsUnknown ensures a scanner returning -1
// ("unknown") leaves the snapshot untouched rather than zeroing it.
func TestQuotaWorker_PollTickSkipsUnknown(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")
	svc := NewService(Config{Pool: pool})
	if err := svc.UpsertQuota(ctx, tenant, 1<<30, 1); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	if err := svc.SetStorageUsage(ctx, tenant, 9000); err != nil {
		t.Fatalf("SetStorageUsage: %v", err)
	}

	scanner := fakeScanner{bytes: map[string]int64{tenant: -1}}
	w := NewQuotaWorker(QuotaWorkerConfig{
		Pool: pool, Billing: svc, Scanner: scanner, Mode: ModePoll, Logger: quietLogger(),
	})
	if err := w.tick(ctx); err != nil {
		t.Fatalf("poll tick: %v", err)
	}
	q, err := svc.GetQuota(ctx, tenant)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.StorageUsedBytes != 9000 {
		t.Errorf("unknown scan should preserve snapshot; got %d want 9000", q.StorageUsedBytes)
	}
}

func TestStaticScanner(t *testing.T) {
	s := StaticScanner{Bytes: 12345}
	got, err := s.ScanTenantBytes(context.Background(), "any")
	if err != nil || got != 12345 {
		t.Fatalf("StaticScanner = (%d,%v) want (12345,nil)", got, err)
	}
}

func TestNewQuotaWorkerMetricsRegisters(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewQuotaWorkerMetrics(reg)
	if m == nil || m.DriftBytes == nil {
		t.Fatal("metrics not built")
	}
	m.DriftBytes.WithLabelValues("t1").Set(5)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "kmail_storage_event_drift_bytes" {
			found = true
		}
	}
	if !found {
		t.Error("drift metric not registered")
	}
}
