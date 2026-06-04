package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kennguy3n/kmail/internal/audit"
)

// fakeScanner is an in-memory StorageScanner. Per-tenant byte counts
// come from `bytes`; tenants in `errs` return an error; tenants absent
// from both return -1 ("unknown").
type fakeScanner struct {
	bytes map[string]int64
	errs  map[string]error
}

func (f fakeScanner) ScanTenantBytes(_ context.Context, tenantID string) (int64, error) {
	if err, ok := f.errs[tenantID]; ok {
		return 0, err
	}
	if b, ok := f.bytes[tenantID]; ok {
		return b, nil
	}
	return -1, nil
}

func TestQuotaWorker_ModeResolution(t *testing.T) {
	events := NewStorageEventService(nil)

	t.Run("explicit event wins", func(t *testing.T) {
		w := NewQuotaWorker(QuotaWorkerConfig{Mode: ModeEvent, Events: events})
		if w.Mode() != ModeEvent {
			t.Fatalf("mode = %q, want event", w.Mode())
		}
		if w.interval != defaultDriftInterval {
			t.Fatalf("interval = %v, want %v", w.interval, defaultDriftInterval)
		}
	})

	t.Run("env enables event", func(t *testing.T) {
		t.Setenv(storageEventsEnabledEnv, "true")
		w := NewQuotaWorker(QuotaWorkerConfig{Events: events})
		if w.Mode() != ModeEvent {
			t.Fatalf("mode = %q, want event", w.Mode())
		}
	})

	t.Run("env disabled falls back to poll", func(t *testing.T) {
		t.Setenv(storageEventsEnabledEnv, "")
		w := NewQuotaWorker(QuotaWorkerConfig{Events: events})
		if w.Mode() != ModePoll {
			t.Fatalf("mode = %q, want poll", w.Mode())
		}
		if w.interval != defaultPollInterval {
			t.Fatalf("interval = %v, want %v", w.interval, defaultPollInterval)
		}
	})

	t.Run("event without store degrades to poll", func(t *testing.T) {
		w := NewQuotaWorker(QuotaWorkerConfig{Mode: ModeEvent}) // Events nil
		if w.Mode() != ModePoll {
			t.Fatalf("mode = %q, want poll", w.Mode())
		}
	})

	t.Run("explicit intervals override", func(t *testing.T) {
		w := NewQuotaWorker(QuotaWorkerConfig{Mode: ModeEvent, Events: events, DriftInterval: 90 * time.Second})
		if w.interval != 90*time.Second {
			t.Fatalf("drift interval = %v, want 90s", w.interval)
		}
		p := NewQuotaWorker(QuotaWorkerConfig{Mode: ModePoll, Interval: 30 * time.Second})
		if p.interval != 30*time.Second {
			t.Fatalf("poll interval = %v, want 30s", p.interval)
		}
	})
}

// TestQuotaWorker_DriftSweep records an event-sourced total, points the
// scanner at a divergent S3 total, and asserts the sweep exposes the
// drift gauge + audit row without mutating the snapshot.
func TestQuotaWorker_DriftSweep(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")

	events := NewStorageEventService(pool)
	// Event-sourced total = 300 (3x100).
	for i := 0; i < 3; i++ {
		mustRecord(t, events, tenant, EventObjectCreated, 100)
	}
	scanner := fakeScanner{bytes: map[string]int64{tenant: 500}} // S3 says 500
	metrics := NewQuotaWorkerMetrics(nil)
	auditSvc := newAuditService(pool)

	w := NewQuotaWorker(QuotaWorkerConfig{
		Pool:    pool,
		Billing: NewService(Config{Pool: pool}),
		Scanner: scanner,
		Events:  events,
		Mode:    ModeEvent,
		Metrics: metrics,
		Audit:   auditSvc,
		Logger:  quietLogger(),
	})
	if w.Mode() != ModeEvent {
		t.Fatalf("expected event mode")
	}
	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := gaugeValue(t, metrics.DriftBytes.WithLabelValues(tenant)); got != 200 {
		t.Fatalf("drift gauge = %v, want 200", got)
	}

	// Drift sweep must NOT write a quotas snapshot in event mode.
	if _, err := w.cfg.Billing.GetQuota(ctx, tenant); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetQuota after drift sweep: want ErrNotFound (no write), got %v", err)
	}

	// A discrepancy must be audited.
	if n := countAudit(t, pool, tenant, "storage.drift_detected"); n != 1 {
		t.Fatalf("drift audit rows = %d, want 1", n)
	}
}

// TestQuotaWorker_DriftSweep_ContinuesOnError ensures a scanner error
// for one tenant is logged and skipped without aborting the sweep or
// recording a spurious gauge.
func TestQuotaWorker_DriftSweep_ContinuesOnError(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	bad := seedTenant(t, pool, "active")

	events := NewStorageEventService(pool)
	scanner := fakeScanner{errs: map[string]error{bad: errors.New("s3 down")}}
	metrics := NewQuotaWorkerMetrics(nil)

	w := NewQuotaWorker(QuotaWorkerConfig{
		Pool:    pool,
		Billing: NewService(Config{Pool: pool}),
		Scanner: scanner,
		Events:  events,
		Mode:    ModeEvent,
		Metrics: metrics,
		Logger:  quietLogger(),
	})
	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick should not fail on a per-tenant scan error: %v", err)
	}
	// No gauge sample should have been recorded for the failed tenant.
	if got := collectCount(metrics.DriftBytes); got != 0 {
		t.Fatalf("drift gauge samples = %d, want 0", got)
	}
}

// TestQuotaWorker_DriftSweep_PrunesDeletedTenantLabels ensures the
// per-tenant drift gauge label is removed once a tenant leaves the
// active set, so kmail_storage_event_drift_bytes does not leak
// cardinality for deleted tenants over the process lifetime.
func TestQuotaWorker_DriftSweep_PrunesDeletedTenantLabels(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")

	events := NewStorageEventService(pool)
	mustRecord(t, events, tenant, EventObjectCreated, 100)
	scanner := fakeScanner{bytes: map[string]int64{tenant: 300}}
	metrics := NewQuotaWorkerMetrics(nil)

	w := NewQuotaWorker(QuotaWorkerConfig{
		Pool:    pool,
		Billing: NewService(Config{Pool: pool}),
		Scanner: scanner,
		Events:  events,
		Mode:    ModeEvent,
		Metrics: metrics,
		Logger:  quietLogger(),
	})

	// First sweep: tenant active → one gauge sample recorded.
	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if got := collectCount(metrics.DriftBytes); got != 1 {
		t.Fatalf("gauge samples after first sweep = %d, want 1", got)
	}

	// Tenant leaves the active set (deleted).
	if _, err := pool.Exec(ctx, `UPDATE tenants SET status='deleted' WHERE id=$1::uuid`, tenant); err != nil {
		t.Fatalf("mark tenant deleted: %v", err)
	}

	// Second sweep: the stale label must be pruned.
	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := collectCount(metrics.DriftBytes); got != 0 {
		t.Fatalf("gauge samples after tenant deleted = %d, want 0 (pruned)", got)
	}
}

// gaugeValue reads the current value of a single Gauge via the
// client_model serialisation (avoids a test-only testutil dependency).
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("gauge.Write: %v", err)
	}
	return m.GetGauge().GetValue()
}

// collectCount returns the number of metric samples a collector emits.
func collectCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	n := 0
	for range ch {
		n++
	}
	return n
}

func newAuditService(pool *pgxpool.Pool) *audit.Service {
	return audit.NewService(pool)
}

// countAudit returns the number of audit_log rows for a tenant with
// the given action (test role bypasses RLS).
func countAudit(t *testing.T, pool *pgxpool.Pool, tenantID, action string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1::uuid AND action = $2`,
		tenantID, action,
	).Scan(&n); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	return n
}
