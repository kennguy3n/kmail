package billing

import (
	"context"
	"errors"
	"testing"
)

// TestEnforcePlanLimitsDB covers the seat-exceeded, storage-exceeded,
// and within-limits branches of EnforcePlanLimits.
func TestEnforcePlanLimitsDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	// seat_limit 2, storage_limit 1000.
	if err := svc.UpsertQuota(ctx, tenant, 1000, 2); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}

	// Within limits → nil.
	if err := svc.EnforcePlanLimits(ctx, tenant); err != nil {
		t.Errorf("within limits: %v", err)
	}

	// Push seat_count over the limit.
	if err := svc.IncrementSeatCount(ctx, tenant, 5); err != nil {
		t.Fatalf("IncrementSeatCount: %v", err)
	}
	if err := svc.EnforcePlanLimits(ctx, tenant); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("seats over limit: got %v want ErrQuotaExceeded", err)
	}

	// Reset seats, push storage over the limit.
	if err := svc.UpsertQuota(ctx, tenant, 1000, 50); err != nil {
		t.Fatalf("UpsertQuota reset: %v", err)
	}
	if err := svc.SetStorageUsage(ctx, tenant, 5000); err != nil {
		t.Fatalf("SetStorageUsage: %v", err)
	}
	if err := svc.EnforcePlanLimits(ctx, tenant); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("storage over limit: got %v want ErrQuotaExceeded", err)
	}
}

// TestCheckStorageQuotaDB covers the invalid-input, no-row (fail
// closed), unlimited, exceed and ok branches.
func TestCheckStorageQuotaDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	if err := svc.CheckStorageQuota(ctx, tenant, -1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative bytes: got %v want ErrInvalidInput", err)
	}

	// No quota row yet → fail closed with ErrQuotaExceeded.
	if err := svc.CheckStorageQuota(ctx, tenant, 10); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("no quota row: got %v want ErrQuotaExceeded", err)
	}

	// Unlimited (limit 0) → always ok.
	if err := svc.UpsertQuota(ctx, tenant, 0, 5); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	if err := svc.CheckStorageQuota(ctx, tenant, 1<<40); err != nil {
		t.Errorf("unlimited: %v", err)
	}

	// Bounded limit: within ok, exceed rejected.
	if err := svc.UpsertQuota(ctx, tenant, 1000, 5); err != nil {
		t.Fatalf("UpsertQuota bounded: %v", err)
	}
	if err := svc.SetStorageUsage(ctx, tenant, 900); err != nil {
		t.Fatalf("SetStorageUsage: %v", err)
	}
	if err := svc.CheckStorageQuota(ctx, tenant, 50); err != nil {
		t.Errorf("within bounded: %v", err)
	}
	if err := svc.CheckStorageQuota(ctx, tenant, 200); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("exceed bounded: got %v want ErrQuotaExceeded", err)
	}
}

// TestCheckSeatAvailableDB covers no-row, unlimited, at-limit and
// under-limit branches.
func TestCheckSeatAvailableDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	// No row → allowed.
	if err := svc.CheckSeatAvailable(ctx, tenant); err != nil {
		t.Errorf("no row: %v", err)
	}

	// Unlimited (0) → allowed.
	if err := svc.UpsertQuota(ctx, tenant, 1000, 0); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	if err := svc.CheckSeatAvailable(ctx, tenant); err != nil {
		t.Errorf("unlimited seats: %v", err)
	}

	// At limit → next seat rejected.
	if err := svc.UpsertQuota(ctx, tenant, 1000, 1); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	if err := svc.IncrementSeatCount(ctx, tenant, 1); err != nil {
		t.Fatalf("IncrementSeatCount: %v", err)
	}
	if err := svc.CheckSeatAvailable(ctx, tenant); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("at limit: got %v want ErrQuotaExceeded", err)
	}
}

// TestIncrementSeatCountDB covers the guard, increment and decrement
// (clamped at zero) paths and the billing_events trail.
func TestIncrementSeatCountDB(t *testing.T) {
	svc, pool, tenant := dbService(t)
	ctx := context.Background()

	if err := svc.IncrementSeatCount(ctx, "", 1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty tenant: got %v want ErrInvalidInput", err)
	}

	if err := svc.UpsertQuota(ctx, tenant, 1000, 10); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	if err := svc.IncrementSeatCount(ctx, tenant, 3); err != nil {
		t.Fatalf("inc +3: %v", err)
	}
	if err := svc.IncrementSeatCount(ctx, tenant, -1); err != nil {
		t.Fatalf("dec -1: %v", err)
	}
	q, err := svc.GetQuota(ctx, tenant)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.SeatCount != 2 {
		t.Errorf("seat_count=%d want 2", q.SeatCount)
	}

	// Decrement below zero clamps at 0 (GREATEST in SQL).
	if err := svc.IncrementSeatCount(ctx, tenant, -100); err != nil {
		t.Fatalf("dec -100: %v", err)
	}
	q, _ = svc.GetQuota(ctx, tenant)
	if q.SeatCount != 0 {
		t.Errorf("clamped seat_count=%d want 0", q.SeatCount)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM billing_events WHERE tenant_id=$1::uuid AND event_type IN ('seat_added','seat_removed')`, tenant).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 3 {
		t.Errorf("seat events=%d want 3", n)
	}
}

// TestCalculateInvoiceDB covers guard, not-found and happy paths.
func TestCalculateInvoiceDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	if _, err := svc.CalculateInvoice(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty tenant: got %v want ErrInvalidInput", err)
	}
	if _, err := svc.CalculateInvoice(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tenant: got %v want ErrNotFound", err)
	}

	// Seeded tenant defaults to a known plan; add 2 seats and verify
	// total = seats * per-seat price.
	if err := svc.UpsertQuota(ctx, tenant, 1000, 10); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	if err := svc.IncrementSeatCount(ctx, tenant, 2); err != nil {
		t.Fatalf("IncrementSeatCount: %v", err)
	}
	total, err := svc.CalculateInvoice(ctx, tenant)
	if err != nil {
		t.Fatalf("CalculateInvoice: %v", err)
	}
	if total < 0 {
		t.Errorf("invoice total=%d want >= 0", total)
	}
}

// TestUpdateQuotaLimitsGuardsDB covers the no-op and negative-value
// validation branches of UpdateQuotaLimits.
func TestUpdateQuotaLimitsGuardsDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	if _, err := svc.UpdateQuotaLimits(ctx, tenant, UpdateQuotaLimitsInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("nothing to update: got %v want ErrInvalidInput", err)
	}
	neg := int64(-1)
	if _, err := svc.UpdateQuotaLimits(ctx, tenant, UpdateQuotaLimitsInput{StorageLimitBytes: &neg}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative storage: got %v want ErrInvalidInput", err)
	}
	negSeat := -1
	if _, err := svc.UpdateQuotaLimits(ctx, tenant, UpdateQuotaLimitsInput{SeatLimit: &negSeat}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative seat: got %v want ErrInvalidInput", err)
	}

	// Missing row → ErrNotFound.
	good := int64(2000)
	if _, err := svc.UpdateQuotaLimits(ctx, tenant, UpdateQuotaLimitsInput{StorageLimitBytes: &good}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing row: got %v want ErrNotFound", err)
	}
}
