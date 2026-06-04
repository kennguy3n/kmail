package billing

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kennguy3n/kmail/internal/audit"
)

// ReconciliationMode selects how the QuotaWorker keeps
// `quotas.storage_used_bytes` honest.
type ReconciliationMode string

const (
	// ModePoll is the legacy behavior: every tick re-scans each
	// tenant's actual S3 footprint and writes it back as the
	// authoritative snapshot.
	ModePoll ReconciliationMode = "poll"
	// ModeEvent is the event-sourced behavior: the StorageEventWorker
	// is the steady-state writer (folding `storage_events` into
	// `quotas`), and the QuotaWorker tick degrades to an hourly
	// drift-correction sweep that compares the event-sourced total to
	// an authoritative S3 scan and surfaces any discrepancy via
	// `kmail_storage_event_drift_bytes` + the audit log.
	ModeEvent ReconciliationMode = "event"
)

// storageEventsEnabledEnv toggles the default reconciliation mode.
// When set to "true" the worker defaults to ModeEvent; otherwise it
// falls back to ModePoll. An explicit Config.Mode always wins.
const storageEventsEnabledEnv = "KMAIL_STORAGE_EVENTS_ENABLED"

// defaultPollInterval and defaultDriftInterval are the per-mode tick
// cadences applied when the caller leaves them unset.
const (
	defaultPollInterval  = 5 * time.Minute
	defaultDriftInterval = time.Hour
)

// StorageScanner is the narrow surface the QuotaWorker relies on to
// compute a tenant's actual storage usage. In production it is
// satisfied by `internal/zkfabric.S3Scanner` (ListObjectsV2 against
// the tenant bucket); tests wire an in-memory fake.
type StorageScanner interface {
	// ScanTenantBytes returns the total size, in bytes, of every
	// object the tenant owns in the shared blob store. Callers
	// treat -1 as "unknown" and skip the snapshot update on that
	// tenant for this tick.
	ScanTenantBytes(ctx context.Context, tenantID string) (int64, error)
}

// QuotaWorkerMetrics is the Prometheus metric set the QuotaWorker
// exposes for its drift-correction sweep.
type QuotaWorkerMetrics struct {
	// DriftBytes is the signed difference (S3 scan minus
	// event-sourced total) observed on the last drift sweep, labeled
	// by tenant. A persistently non-zero value means the event stream
	// has diverged from physical storage and warrants investigation.
	DriftBytes *prometheus.GaugeVec
}

// NewQuotaWorkerMetrics builds the metric set and registers it with
// `reg`. Pass nil to skip registration (tests).
func NewQuotaWorkerMetrics(reg prometheus.Registerer) *QuotaWorkerMetrics {
	m := &QuotaWorkerMetrics{
		DriftBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kmail_storage_event_drift_bytes",
			Help: "Signed drift (S3 scan minus event-sourced total) per tenant on the last drift sweep.",
		}, []string{"tenant_id"}),
	}
	if reg != nil {
		reg.MustRegister(m.DriftBytes)
	}
	return m
}

// QuotaWorkerConfig wires the background worker.
type QuotaWorkerConfig struct {
	Pool     *pgxpool.Pool
	Billing  *Service
	Scanner  StorageScanner
	Interval time.Duration
	Logger   *log.Logger

	// Mode selects poll (legacy) or event (drift-correction)
	// reconciliation. When empty it is derived from
	// KMAIL_STORAGE_EVENTS_ENABLED.
	Mode ReconciliationMode
	// Events supplies the event-sourced total in ModeEvent. Required
	// for event mode; if nil the worker falls back to ModePoll.
	Events *StorageEventService
	// DriftInterval overrides the ModeEvent sweep cadence (default 1h).
	DriftInterval time.Duration
	// Audit, when set, records a system entry whenever a drift sweep
	// detects a non-zero discrepancy.
	Audit *audit.Service
	// Metrics, when set, exposes kmail_storage_event_drift_bytes.
	Metrics *QuotaWorkerMetrics
}

// QuotaWorker is a background goroutine that keeps
// `quotas.storage_used_bytes` authoritative.
//
// In ModePoll it polls zk-object-fabric once per `Interval`, sums each
// tenant's actual storage footprint, and writes the snapshot back so
// the admin console and `CheckStorageQuota` see a correct value even
// if the delta counter drifts (crash, restart, orphaned blobs from a
// failed Stalwart submission).
//
// In ModeEvent the StorageEventWorker is the steady-state writer and
// this worker instead runs an hourly drift-correction sweep: it
// compares the event-sourced total against an authoritative S3 scan,
// records `kmail_storage_event_drift_bytes`, and audit-logs any
// discrepancy without mutating the snapshot.
type QuotaWorker struct {
	cfg      QuotaWorkerConfig
	mode     ReconciliationMode
	interval time.Duration
}

// NewQuotaWorker builds a worker with sensible defaults. The
// reconciliation mode is resolved from Config.Mode, falling back to
// KMAIL_STORAGE_EVENTS_ENABLED, then to ModePoll. The tick cadence
// defaults to 5m in poll mode and 1h in event mode.
func NewQuotaWorker(cfg QuotaWorkerConfig) *QuotaWorker {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	mode := resolveMode(cfg)
	interval := resolveInterval(cfg, mode)
	return &QuotaWorker{cfg: cfg, mode: mode, interval: interval}
}

// Mode reports the reconciliation mode the worker resolved at
// construction. Exposed for tests and operational introspection.
func (w *QuotaWorker) Mode() ReconciliationMode { return w.mode }

func resolveMode(cfg QuotaWorkerConfig) ReconciliationMode {
	mode := cfg.Mode
	if mode == "" {
		if os.Getenv(storageEventsEnabledEnv) == "true" {
			mode = ModeEvent
		} else {
			mode = ModePoll
		}
	}
	// Event mode needs the event store to read from. Without it there
	// is nothing to compare, so degrade to poll rather than no-op.
	if mode == ModeEvent && cfg.Events == nil {
		cfg.Logger.Printf("quota worker: event mode requested without an event store, falling back to poll")
		return ModePoll
	}
	return mode
}

func resolveInterval(cfg QuotaWorkerConfig, mode ReconciliationMode) time.Duration {
	if mode == ModeEvent {
		if cfg.DriftInterval > 0 {
			return cfg.DriftInterval
		}
		return defaultDriftInterval
	}
	if cfg.Interval > 0 {
		return cfg.Interval
	}
	return defaultPollInterval
}

// Run loops until `ctx` is cancelled, ticking on the resolved
// interval. Failures on a single tenant are logged and the loop
// continues so one bad bucket does not starve the rest.
func (w *QuotaWorker) Run(ctx context.Context) {
	if w.cfg.Pool == nil || w.cfg.Billing == nil || w.cfg.Scanner == nil {
		w.cfg.Logger.Printf("quota worker: not configured, exiting")
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	// Kick once immediately so the very first boot doesn't wait a
	// full interval before the admin UI reflects storage usage.
	if err := w.tick(ctx); err != nil {
		w.cfg.Logger.Printf("quota worker first tick: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.tick(ctx); err != nil {
				w.cfg.Logger.Printf("quota worker tick: %v", err)
			}
		}
	}
}

func (w *QuotaWorker) tick(ctx context.Context) error {
	if w.mode == ModeEvent {
		return w.driftTick(ctx)
	}
	return w.pollTick(ctx)
}

// pollTick is the legacy reconciliation: re-scan each tenant and write
// the authoritative snapshot.
func (w *QuotaWorker) pollTick(ctx context.Context) error {
	ids, err := listActiveTenantIDs(ctx, w.cfg.Pool)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	for _, id := range ids {
		bytes, err := w.cfg.Scanner.ScanTenantBytes(ctx, id)
		if err != nil {
			w.cfg.Logger.Printf("quota worker: scan tenant %s: %v", id, err)
			continue
		}
		if bytes < 0 {
			continue
		}
		if err := w.cfg.Billing.SetStorageUsage(ctx, id, bytes); err != nil {
			w.cfg.Logger.Printf("quota worker: set usage tenant %s: %v", id, err)
			continue
		}
	}
	return nil
}

// driftTick is the event-mode sweep: compare the event-sourced total
// to the authoritative S3 scan, expose the per-tenant drift gauge, and
// audit-log any discrepancy. It never mutates the snapshot — the
// StorageEventWorker owns writes in event mode.
func (w *QuotaWorker) driftTick(ctx context.Context) error {
	ids, err := listActiveTenantIDs(ctx, w.cfg.Pool)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	for _, id := range ids {
		actual, err := w.cfg.Scanner.ScanTenantBytes(ctx, id)
		if err != nil {
			w.cfg.Logger.Printf("quota worker: drift scan tenant %s: %v", id, err)
			continue
		}
		if actual < 0 {
			// Scanner reports "unknown" — skip this tenant rather than
			// record a spurious drift against a zero baseline.
			continue
		}
		eventTotal, err := w.cfg.Events.ReconcileTenant(ctx, id)
		if err != nil {
			w.cfg.Logger.Printf("quota worker: drift reconcile tenant %s: %v", id, err)
			continue
		}
		drift := actual - eventTotal
		if w.cfg.Metrics != nil {
			w.cfg.Metrics.DriftBytes.WithLabelValues(id).Set(float64(drift))
		}
		if drift != 0 {
			w.cfg.Logger.Printf("quota worker: storage drift tenant %s: s3=%d event=%d drift=%d",
				id, actual, eventTotal, drift)
			w.auditDrift(ctx, id, actual, eventTotal, drift)
		}
	}
	return nil
}

func (w *QuotaWorker) auditDrift(ctx context.Context, tenantID string, actual, eventTotal, drift int64) {
	if w.cfg.Audit == nil {
		return
	}
	if _, err := w.cfg.Audit.Log(ctx, audit.Entry{
		TenantID:     tenantID,
		ActorID:      "quota-worker",
		ActorType:    audit.ActorSystem,
		Action:       "storage.drift_detected",
		ResourceType: "quota",
		ResourceID:   tenantID,
		Metadata: map[string]any{
			"s3_bytes":    actual,
			"event_bytes": eventTotal,
			"drift_bytes": drift,
		},
	}); err != nil {
		w.cfg.Logger.Printf("quota worker: audit drift tenant %s: %v", tenantID, err)
	}
}

// listActiveTenantIDs returns the IDs of every tenant not in status
// 'deleted'. Shared by the QuotaWorker and StorageEventWorker.
func listActiveTenantIDs(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text FROM tenants WHERE status <> 'deleted'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// StaticScanner is a StorageScanner that always returns the same
// byte count. Useful as a no-op fallback when the zk-object-fabric
// client is not wired (local dev, CI) so the worker path is still
// exercised without real S3 calls.
type StaticScanner struct {
	Bytes int64
}

// ScanTenantBytes implements StorageScanner.
func (s StaticScanner) ScanTenantBytes(_ context.Context, _ string) (int64, error) {
	return s.Bytes, nil
}
