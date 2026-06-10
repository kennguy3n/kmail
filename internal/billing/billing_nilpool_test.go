package billing

import (
	"context"
	"errors"
	"testing"
)

// TestServiceNilPoolBranches verifies every Service method degrades
// gracefully when no pool is configured (stub mode), exercising the
// `s.cfg.Pool == nil` short-circuits.
func TestServiceNilPoolBranches(t *testing.T) {
	svc := NewService(Config{})
	ctx := context.Background()
	const tenant = "t-nilpool"

	if _, err := svc.GetQuota(ctx, tenant); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetQuota nil pool: %v", err)
	}
	if err := svc.UpsertQuota(ctx, tenant, 1, 1); err != nil {
		t.Errorf("UpsertQuota nil pool: %v", err)
	}
	if _, err := svc.UpdateQuotaLimits(ctx, tenant, UpdateQuotaLimitsInput{SeatLimit: intPtr(3)}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateQuotaLimits nil pool: %v", err)
	}
	if err := svc.UpdateStorageUsage(ctx, tenant, 1); err != nil {
		t.Errorf("UpdateStorageUsage nil pool: %v", err)
	}
	if err := svc.SetStorageUsage(ctx, tenant, 1); err != nil {
		t.Errorf("SetStorageUsage nil pool: %v", err)
	}
	if n, err := svc.CountSeats(ctx, tenant); err != nil || n != 0 {
		t.Errorf("CountSeats nil pool: n=%d err=%v", n, err)
	}
	if n, err := svc.SyncSeatCount(ctx, tenant); err != nil || n != 0 {
		t.Errorf("SyncSeatCount nil pool: n=%d err=%v", n, err)
	}
	if err := svc.IncrementSeatCount(ctx, tenant, 1); err != nil {
		t.Errorf("IncrementSeatCount nil pool: %v", err)
	}
	if c, err := svc.CalculateInvoice(ctx, tenant); err != nil || c != 0 {
		t.Errorf("CalculateInvoice nil pool: c=%d err=%v", c, err)
	}
	if _, err := svc.Summary(ctx, tenant); !errors.Is(err, ErrNotFound) {
		t.Errorf("Summary nil pool: %v", err)
	}
	if _, err := svc.ChangePlan(ctx, tenant, PlanPro); !errors.Is(err, ErrNotFound) {
		t.Errorf("ChangePlan nil pool: %v", err)
	}
}

// TestLifecycleNilPoolBranches covers the lifecycle short-circuits
// against a pool-less Service.
func TestLifecycleNilPoolBranches(t *testing.T) {
	lc := NewLifecycle(NewService(Config{}), nil)
	ctx := context.Background()
	const tenant = "t-nilpool"

	if _, err := lc.GetSubscription(ctx, tenant); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSubscription nil pool: %v", err)
	}
	if out, err := lc.ListBillingHistory(ctx, tenant, 10); err != nil || out != nil {
		t.Errorf("ListBillingHistory nil pool: out=%v err=%v", out, err)
	}
	// OnTenantCreated still validates plan and runs UpsertQuota (no-op).
	if err := lc.OnTenantCreated(ctx, tenant, "bogus"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("OnTenantCreated bad plan: %v", err)
	}
	if err := lc.OnTenantCreated(ctx, tenant, PlanPro); err != nil {
		t.Errorf("OnTenantCreated nil pool: %v", err)
	}
	if err := lc.OnSeatAdded(ctx, tenant); err != nil {
		t.Errorf("OnSeatAdded nil pool: %v", err)
	}
	if err := lc.OnSeatRemoved(ctx, tenant); err != nil {
		t.Errorf("OnSeatRemoved nil pool: %v", err)
	}

	// Nil-receiver lifecycle methods are safe no-ops.
	var nilLC *Lifecycle
	if err := nilLC.OnTenantCreated(ctx, tenant, PlanPro); err != nil {
		t.Errorf("nil lifecycle OnTenantCreated: %v", err)
	}
	if err := nilLC.OnSeatAdded(ctx, tenant); err != nil {
		t.Errorf("nil lifecycle OnSeatAdded: %v", err)
	}
}

func intPtr(v int) *int { return &v }
