package billing

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Storage event types. These mirror the CHECK constraint on
// `storage_events.event_type` (see migrations/005_storage_events.sql).
// zk-object-fabric emits S3-compatible object lifecycle notifications
// (`s3:ObjectCreated:*` / `s3:ObjectRemoved:*`) which the billing
// webhook normalises to one of these two canonical values before
// recording a row.
const (
	// EventObjectCreated records that an object was written; its
	// `size_bytes` is *added* to the tenant's event-sourced total.
	EventObjectCreated = "object_created"
	// EventObjectDeleted records that an object was removed; its
	// `size_bytes` is *subtracted* from the event-sourced total.
	// Callers store the (positive) size of the object that was
	// deleted — the reconciliation SUM applies the sign.
	EventObjectDeleted = "object_deleted"
)

// validStorageEventType reports whether `t` is one of the canonical
// event types the `storage_events.event_type` CHECK constraint
// accepts. Rejecting unknown values in Go (rather than relying on the
// constraint to raise) lets RecordEvent surface ErrInvalidInput, which
// handlers map to HTTP 400 instead of a 500 from the failed INSERT.
func validStorageEventType(t string) bool {
	return t == EventObjectCreated || t == EventObjectDeleted
}

// StorageEventService is the event-sourced storage-accounting store.
// It appends one row per object lifecycle event to `storage_events`
// and reconciles a tenant's authoritative usage by summing created
// minus deleted bytes. The StorageEventWorker periodically folds the
// reconciled total into `quotas.storage_used_bytes` so the admin
// console and `CheckStorageQuota` observe an event-sourced value that
// no longer depends on the delta counter staying perfectly in sync
// with every mail write / deletion.
type StorageEventService struct {
	pool *pgxpool.Pool
}

// NewStorageEventService builds a StorageEventService bound to the
// control-plane pool. A nil pool is tolerated (local dev / CI without
// Postgres): RecordEvent becomes a no-op and ReconcileTenant returns
// a zero total, mirroring the nil-pool handling on Service.
func NewStorageEventService(pool *pgxpool.Pool) *StorageEventService {
	return &StorageEventService{pool: pool}
}

// RecordEvent appends a single object lifecycle event for `tenantID`.
// `sizeBytes` must be non-negative (the magnitude of the object); the
// sign is applied at reconciliation time based on `eventType`. The
// insert runs inside a tenant-scoped transaction so the permissive
// RLS policy on `storage_events` still pins the row to `tenantID`.
func (s *StorageEventService) RecordEvent(ctx context.Context, tenantID, eventType, objectKey string, sizeBytes int64) error {
	return s.RecordEvents(ctx, tenantID, []StorageEvent{{
		EventType: eventType,
		ObjectKey: objectKey,
		SizeBytes: sizeBytes,
	}})
}

// StorageEvent is a single object lifecycle event to append for a
// tenant. It is the batch unit consumed by RecordEvents.
type StorageEvent struct {
	EventType string
	ObjectKey string
	SizeBytes int64
}

// RecordEvents appends a batch of lifecycle events for `tenantID` in a
// single tenant-scoped transaction, so the whole batch commits or
// rolls back atomically. Atomicity is what makes the storage-event
// webhook safe under at-least-once delivery: if any row in a delivered
// batch fails, none are persisted, so the producer's retry re-inserts
// the batch exactly once instead of duplicating the rows that happened
// to succeed before the failure (which would inflate the tenant's
// event-sourced total until the next drift sweep). All events are
// validated up front; an invalid event fails the whole batch with
// ErrInvalidInput before any write. A nil pool or empty batch is a
// no-op.
func (s *StorageEventService) RecordEvents(ctx context.Context, tenantID string, events []StorageEvent) error {
	if tenantID == "" {
		return fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	for _, e := range events {
		if !validStorageEventType(e.EventType) {
			return fmt.Errorf("%w: unknown storage event type %q", ErrInvalidInput, e.EventType)
		}
		if e.SizeBytes < 0 {
			return fmt.Errorf("%w: sizeBytes must be >= 0", ErrInvalidInput)
		}
	}
	if s.pool == nil || len(events) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		for _, e := range events {
			if _, err := tx.Exec(ctx, `
				INSERT INTO storage_events (tenant_id, event_type, object_key, size_bytes)
				VALUES ($1::uuid, $2, $3, $4)
			`, tenantID, e.EventType, e.ObjectKey, e.SizeBytes); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReconcileTenant returns the event-sourced storage total for
// `tenantID`: the sum of `object_created` bytes minus `object_deleted`
// bytes across every recorded event. The result can momentarily go
// negative if a delete is observed before its matching create (event
// reordering); callers that persist the value into `quotas`
// (which CHECKs storage_used_bytes >= 0) must clamp at zero.
func (s *StorageEventService) ReconcileTenant(ctx context.Context, tenantID string) (int64, error) {
	if tenantID == "" {
		return 0, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return 0, nil
	}
	var total int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(
				CASE WHEN event_type = 'object_created'
				     THEN size_bytes
				     ELSE -size_bytes
				END
			), 0)
			FROM storage_events
			WHERE tenant_id = $1::uuid
		`, tenantID).Scan(&total)
	})
	if err != nil {
		return 0, fmt.Errorf("reconcile tenant %s: %w", tenantID, err)
	}
	return total, nil
}
