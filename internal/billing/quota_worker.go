package billing

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// QuotaWorkerConfig wires the background worker.
type QuotaWorkerConfig struct {
	Pool     *pgxpool.Pool
	Billing  *Service
	Scanner  StorageScanner
	Interval time.Duration
	Logger   *log.Logger
}

// QuotaWorker is a background goroutine that polls zk-object-fabric
// once per `Interval`, sums each tenant's actual storage footprint,
// and writes the snapshot back into `quotas.storage_used_bytes` so
// the admin console and `CheckStorageQuota` see an authoritative
// value even if the delta counter drifts (crash, restart, orphaned
// blobs from a failed Stalwart submission).
type QuotaWorker struct {
	cfg QuotaWorkerConfig
}

// NewQuotaWorker builds a worker with sensible defaults. When
// `cfg.Interval` is zero the worker polls every 5 minutes.
func NewQuotaWorker(cfg QuotaWorkerConfig) *QuotaWorker {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &QuotaWorker{cfg: cfg}
}

// Run loops until `ctx` is cancelled. It ticks on the configured
// interval, pulls every tenant ID out of Postgres, and asks the
// scanner for each tenant's current byte count. Failures on a
// single tenant are logged and the loop continues so one bad bucket
// does not starve the rest.
func (w *QuotaWorker) Run(ctx context.Context) {
	if w.cfg.Pool == nil || w.cfg.Billing == nil || w.cfg.Scanner == nil {
		w.cfg.Logger.Printf("quota worker: not configured, exiting")
		return
	}
	ticker := time.NewTicker(w.cfg.Interval)
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
	ids, err := w.listTenantIDs(ctx)
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

func (w *QuotaWorker) listTenantIDs(ctx context.Context) ([]string, error) {
	rows, err := w.cfg.Pool.Query(ctx, `
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
