package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestChangePlanDB covers same-plan no-op, an actual plan change with
// the per-seat storage default sync, and the refreshed summary.
func TestChangePlanDB(t *testing.T) {
	svc, _, tenant := dbService(t) // seeded plan = pro
	ctx := context.Background()

	// Seed quota with the pro per-seat default so ChangePlan's
	// storage-default sync branch is exercised on downgrade.
	proPerSeat, _ := svc.PerSeatStorageBytes(PlanPro)
	if err := svc.UpsertQuota(ctx, tenant, proPerSeat, 10); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}

	// Same-plan change is a no-op that still returns a summary.
	sum, err := svc.ChangePlan(ctx, tenant, PlanPro)
	if err != nil {
		t.Fatalf("ChangePlan same: %v", err)
	}
	if sum.Plan != PlanPro {
		t.Errorf("same-plan summary plan=%s want pro", sum.Plan)
	}

	// Change pro → core: tenants.plan updates and the storage default
	// is re-synced because the quota still held the pro default.
	sum, err = svc.ChangePlan(ctx, tenant, PlanCore)
	if err != nil {
		t.Fatalf("ChangePlan core: %v", err)
	}
	if sum.Plan != PlanCore {
		t.Errorf("summary plan=%s want core", sum.Plan)
	}
	corePerSeat, _ := svc.PerSeatStorageBytes(PlanCore)
	q, err := svc.GetQuota(ctx, tenant)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.StorageLimitBytes != corePerSeat {
		t.Errorf("storage limit=%d want core default %d", q.StorageLimitBytes, corePerSeat)
	}

	// Guards.
	if _, err := svc.ChangePlan(ctx, "", PlanPro); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty tenant: got %v want ErrInvalidInput", err)
	}
	if _, err := svc.ChangePlan(ctx, tenant, "bogus"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad plan: got %v want ErrInvalidInput", err)
	}
	if _, err := svc.ChangePlan(ctx, "00000000-0000-0000-0000-000000000000", PlanPro); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tenant: got %v want ErrNotFound", err)
	}
}

// TestChangePlanDowngradeExceedsQuotaDB verifies EnforcePlanLimits runs
// after the commit and surfaces ErrQuotaExceeded when the post-change
// quota no longer fits current usage.
func TestChangePlanDowngradeExceedsQuotaDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	// Tight seat limit already exceeded by seat_count.
	if err := svc.UpsertQuota(ctx, tenant, 1<<40, 1); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	if err := svc.IncrementSeatCount(ctx, tenant, 5); err != nil {
		t.Fatalf("IncrementSeatCount: %v", err)
	}
	if _, err := svc.ChangePlan(ctx, tenant, PlanCore); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("downgrade over seats: got %v want ErrQuotaExceeded", err)
	}
}

// TestSummaryAndCountSeatsDB covers Summary's synthesised-quota branch
// and CountSeats excluding shared/service accounts.
func TestSummaryAndCountSeatsDB(t *testing.T) {
	svc, pool, tenant := dbService(t)
	ctx := context.Background()

	// Summary with no quota row synthesises a zero row.
	sum, err := svc.Summary(ctx, tenant)
	if err != nil {
		t.Fatalf("Summary no-quota: %v", err)
	}
	if sum.Plan != PlanPro || sum.SeatCount != 0 {
		t.Errorf("synthesised summary=%+v", sum)
	}

	// Two human users count as seats; a shared inbox does not.
	seedActiveUser(t, pool, tenant)
	seedActiveUser(t, pool, tenant)
	sharedUniq := fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (tenant_id, kchat_user_id, stalwart_account_id, email, display_name, status, account_type)
		VALUES ($1::uuid, $2, $3, $4, 'Shared', 'active', 'shared_inbox')
	`, tenant, "kc-shared-"+sharedUniq, "sw-shared-"+sharedUniq, "shared-"+sharedUniq+"@example.com"); err != nil {
		t.Fatalf("seed shared inbox: %v", err)
	}
	n, err := svc.CountSeats(ctx, tenant)
	if err != nil {
		t.Fatalf("CountSeats: %v", err)
	}
	if n != 2 {
		t.Errorf("CountSeats=%d want 2 (shared inbox excluded)", n)
	}

	// SyncSeatCount writes the computed count into quotas.
	if err := svc.UpsertQuota(ctx, tenant, 1000, 10); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	synced, err := svc.SyncSeatCount(ctx, tenant)
	if err != nil || synced != 2 {
		t.Fatalf("SyncSeatCount=%d err=%v want 2", synced, err)
	}
	q, _ := svc.GetQuota(ctx, tenant)
	if q.SeatCount != 2 {
		t.Errorf("quota seat_count=%d want 2", q.SeatCount)
	}

	// CountSeats guard.
	if _, err := svc.CountSeats(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("CountSeats empty: got %v want ErrInvalidInput", err)
	}
}

// TestUpdateStorageUsageDB exercises the delta-update path and the
// CHECK(>=0) clamp.
func TestUpdateStorageUsageDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	if err := svc.UpdateStorageUsage(ctx, tenant, 500); err != nil {
		t.Fatalf("UpdateStorageUsage +500: %v", err)
	}
	if err := svc.UpdateStorageUsage(ctx, tenant, -200); err != nil {
		t.Fatalf("UpdateStorageUsage -200: %v", err)
	}
	q, err := svc.GetQuota(ctx, tenant)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.StorageUsedBytes != 300 {
		t.Errorf("used=%d want 300", q.StorageUsedBytes)
	}

	// Over-decrement clamps at 0.
	if err := svc.UpdateStorageUsage(ctx, tenant, -10000); err != nil {
		t.Fatalf("UpdateStorageUsage clamp: %v", err)
	}
	q, _ = svc.GetQuota(ctx, tenant)
	if q.StorageUsedBytes != 0 {
		t.Errorf("clamped used=%d want 0", q.StorageUsedBytes)
	}

	if err := svc.UpdateStorageUsage(ctx, "", 1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty tenant: got %v want ErrInvalidInput", err)
	}
	if err := svc.SetStorageUsage(ctx, tenant, -1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative set: got %v want ErrInvalidInput", err)
	}
}

func TestServicePoolAccessor(t *testing.T) {
	svc, pool, _ := dbService(t)
	if svc.Pool() != pool {
		t.Error("Pool() did not return the configured pool")
	}
}
