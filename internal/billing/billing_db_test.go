package billing

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func dbService(t *testing.T) (*Service, *pgxpool.Pool, string) {
	t.Helper()
	pool := testPool(t)
	tenant := seedTenant(t, pool, "active")
	// billing_subscriptions.tenant_id is ON DELETE RESTRICT and
	// stripe_subscription_id is globally UNIQUE; drop any row this
	// tenant created so (a) the seedTenant tenant-delete cleanup is not
	// blocked and (b) a fixed mock Stripe subscription ID does not
	// collide across sequential tests. Registered after seedTenant so
	// it runs first (LIFO), before the tenant row is deleted.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM billing_subscriptions WHERE tenant_id = $1::uuid`, tenant)
	})
	return NewService(Config{Pool: pool}), pool, tenant
}

var userSeq int64

// seedActiveUser inserts an active human user (counts toward seats).
func seedActiveUser(t *testing.T, pool *pgxpool.Pool, tenantID string) {
	t.Helper()
	n := atomic.AddInt64(&userSeq, 1)
	uniq := fmt.Sprintf("%d-%d", time.Now().UnixNano(), n)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO users (tenant_id, kchat_user_id, stalwart_account_id, email, display_name, status, account_type)
		VALUES ($1::uuid, $2, $3, $4, 'Test User', 'active', 'user')
	`, tenantID, "kc-"+uniq, "sw-"+uniq, "u-"+uniq+"@example.com")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func TestQuotaUpsertGetUpdateDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	// Missing row → ErrNotFound.
	if _, err := svc.GetQuota(ctx, tenant); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetQuota missing=%v want ErrNotFound", err)
	}

	if err := svc.UpsertQuota(ctx, tenant, 1000, 5); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	q, err := svc.GetQuota(ctx, tenant)
	if err != nil || q.StorageLimitBytes != 1000 || q.SeatLimit != 5 {
		t.Fatalf("GetQuota=%+v err=%v", q, err)
	}

	// Upsert again updates in place.
	if err := svc.UpsertQuota(ctx, tenant, 2000, 9); err != nil {
		t.Fatalf("UpsertQuota update: %v", err)
	}

	// Patch only seat limit; storage untouched.
	seat := 7
	updated, err := svc.UpdateQuotaLimits(ctx, tenant, UpdateQuotaLimitsInput{SeatLimit: &seat})
	if err != nil {
		t.Fatalf("UpdateQuotaLimits: %v", err)
	}
	if updated.SeatLimit != 7 || updated.StorageLimitBytes != 2000 {
		t.Errorf("updated=%+v", updated)
	}

	// Validation paths.
	if _, err := svc.UpdateQuotaLimits(ctx, tenant, UpdateQuotaLimitsInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty update want ErrInvalidInput, got %v", err)
	}
	neg := int64(-1)
	if _, err := svc.UpdateQuotaLimits(ctx, tenant, UpdateQuotaLimitsInput{StorageLimitBytes: &neg}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative storage want ErrInvalidInput, got %v", err)
	}
}

func TestStorageUsageDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	if err := svc.UpdateStorageUsage(ctx, tenant, 500); err != nil {
		t.Fatalf("UpdateStorageUsage: %v", err)
	}
	q, _ := svc.GetQuota(ctx, tenant)
	if q.StorageUsedBytes != 500 {
		t.Fatalf("used=%d want 500", q.StorageUsedBytes)
	}

	// Negative delta clamps at zero (CHECK >= 0 + GREATEST).
	if err := svc.UpdateStorageUsage(ctx, tenant, -9999); err != nil {
		t.Fatalf("UpdateStorageUsage negative: %v", err)
	}
	q, _ = svc.GetQuota(ctx, tenant)
	if q.StorageUsedBytes != 0 {
		t.Fatalf("used=%d want 0 after clamp", q.StorageUsedBytes)
	}

	// Absolute set.
	if err := svc.SetStorageUsage(ctx, tenant, 1234); err != nil {
		t.Fatalf("SetStorageUsage: %v", err)
	}
	q, _ = svc.GetQuota(ctx, tenant)
	if q.StorageUsedBytes != 1234 {
		t.Fatalf("used=%d want 1234", q.StorageUsedBytes)
	}
}

func TestSeatCountingDB(t *testing.T) {
	svc, pool, tenant := dbService(t)
	ctx := context.Background()

	seedActiveUser(t, pool, tenant)
	seedActiveUser(t, pool, tenant)

	n, err := svc.CountSeats(ctx, tenant)
	if err != nil || n != 2 {
		t.Fatalf("CountSeats=%d err=%v", n, err)
	}

	if _, err := svc.SyncSeatCount(ctx, tenant); err != nil {
		t.Fatalf("SyncSeatCount: %v", err)
	}
	q, _ := svc.GetQuota(ctx, tenant)
	if q.SeatCount != 2 {
		t.Fatalf("seat_count=%d want 2", q.SeatCount)
	}

	if err := svc.IncrementSeatCount(ctx, tenant, 3); err != nil {
		t.Fatalf("IncrementSeatCount: %v", err)
	}
	q, _ = svc.GetQuota(ctx, tenant)
	if q.SeatCount != 5 {
		t.Fatalf("seat_count=%d want 5", q.SeatCount)
	}
	// Decrement clamps at 0.
	if err := svc.IncrementSeatCount(ctx, tenant, -100); err != nil {
		t.Fatalf("IncrementSeatCount neg: %v", err)
	}
	q, _ = svc.GetQuota(ctx, tenant)
	if q.SeatCount != 0 {
		t.Fatalf("seat_count=%d want 0", q.SeatCount)
	}
}

func TestEnforceAndChecksDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()

	// No quota row: CheckSeatAvailable is permissive, CheckStorageQuota fails closed.
	if err := svc.CheckSeatAvailable(ctx, tenant); err != nil {
		t.Errorf("CheckSeatAvailable no-row=%v want nil", err)
	}
	if err := svc.CheckStorageQuota(ctx, tenant, 1); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("CheckStorageQuota no-row=%v want ErrQuotaExceeded", err)
	}

	if err := svc.UpsertQuota(ctx, tenant, 1000, 2); err != nil {
		t.Fatalf("UpsertQuota: %v", err)
	}
	_ = svc.SetStorageUsage(ctx, tenant, 900)
	_ = svc.IncrementSeatCount(ctx, tenant, 2)

	if err := svc.EnforcePlanLimits(ctx, tenant); err != nil {
		t.Errorf("EnforcePlanLimits at-limit=%v want nil", err)
	}
	// Over storage.
	if err := svc.CheckStorageQuota(ctx, tenant, 200); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("CheckStorageQuota over=%v want ErrQuotaExceeded", err)
	}
	// One more seat would exceed limit 2.
	if err := svc.CheckSeatAvailable(ctx, tenant); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("CheckSeatAvailable over=%v want ErrQuotaExceeded", err)
	}
}

func TestInvoiceSummaryChangePlanDB(t *testing.T) {
	svc, pool, tenant := dbService(t)
	ctx := context.Background()

	seedActiveUser(t, pool, tenant)
	seedActiveUser(t, pool, tenant)
	_ = svc.UpsertQuota(ctx, tenant, 0, 10)
	_, _ = svc.SyncSeatCount(ctx, tenant)

	// tenant seeded as 'pro' → 600 cents/seat * 2 = 1200.
	total, err := svc.CalculateInvoice(ctx, tenant)
	if err != nil || total != 1200 {
		t.Fatalf("CalculateInvoice=%d err=%v want 1200", total, err)
	}

	sum, err := svc.Summary(ctx, tenant)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Plan != "pro" || sum.PerSeatCents != 600 || sum.MonthlyCents != 1200 {
		t.Errorf("summary=%+v", sum)
	}

	// Change plan pro → privacy; per-seat price becomes 900.
	newSum, err := svc.ChangePlan(ctx, tenant, PlanPrivacy)
	if err != nil {
		t.Fatalf("ChangePlan: %v", err)
	}
	if newSum.Plan != PlanPrivacy || newSum.PerSeatCents != 900 {
		t.Errorf("changed summary=%+v", newSum)
	}

	// Unknown plan rejected.
	if _, err := svc.ChangePlan(ctx, tenant, "enterprise"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ChangePlan unknown=%v want ErrInvalidInput", err)
	}
}
