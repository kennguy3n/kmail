package billing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool dials the integration database named by KMAIL_TEST_DATABASE_URL
// (or DATABASE_URL). When neither is set — the default for `make test`
// and CI, which have no Postgres — the calling test is skipped. The
// returned pool is closed via t.Cleanup.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KMAIL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set KMAIL_TEST_DATABASE_URL or DATABASE_URL to run billing storage-event DB tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable (%v); skipping integration test", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var tenantSeq int64

// seedTenant inserts a tenant with the given status and registers
// cleanup that removes it (and its cascading storage_events /
// audit_log / quotas rows). It returns the new tenant UUID.
func seedTenant(t *testing.T, pool *pgxpool.Pool, status string) string {
	t.Helper()
	ctx := context.Background()
	n := atomic.AddInt64(&tenantSeq, 1)
	slug := fmt.Sprintf("se-test-%d-%d", time.Now().UnixNano(), n)
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO tenants (name, slug, plan, status)
		VALUES ($1, $2, 'pro', $3)
		RETURNING id::text
	`, "storage-event-test", slug, status).Scan(&id)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, id)
	})
	return id
}

// countStorageEvents returns the number of storage_events rows for a
// tenant (RLS GUC must allow it — superuser/test role bypasses RLS).
func countStorageEvents(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM storage_events WHERE tenant_id = $1::uuid`, tenantID,
	).Scan(&n); err != nil {
		t.Fatalf("count storage_events: %v", err)
	}
	return n
}

func TestStorageEventService_Validation(t *testing.T) {
	// Pure validation — no database required.
	s := NewStorageEventService(nil)
	ctx := context.Background()

	if err := s.RecordEvent(ctx, "", EventObjectCreated, "k", 1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty tenant: want ErrInvalidInput, got %v", err)
	}
	if err := s.RecordEvent(ctx, "t", "bogus", "k", 1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad event type: want ErrInvalidInput, got %v", err)
	}
	if err := s.RecordEvent(ctx, "t", EventObjectCreated, "k", -1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative size: want ErrInvalidInput, got %v", err)
	}
	// nil pool is a tolerated no-op for valid input.
	if err := s.RecordEvent(ctx, "t", EventObjectCreated, "k", 10); err != nil {
		t.Errorf("nil-pool RecordEvent: want nil, got %v", err)
	}
	if _, err := s.ReconcileTenant(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ReconcileTenant empty tenant: want ErrInvalidInput, got %v", err)
	}
	total, err := s.ReconcileTenant(ctx, "t")
	if err != nil || total != 0 {
		t.Errorf("nil-pool ReconcileTenant: want (0,nil), got (%d,%v)", total, err)
	}
}

// TestStorageEventService_ReconcileMath is the canonical accounting
// check: record 5 creates and 2 deletes, then assert the reconciled
// total equals created-minus-deleted bytes.
func TestStorageEventService_ReconcileMath(t *testing.T) {
	pool := testPool(t)
	svc := NewStorageEventService(pool)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")

	const objBytes = 100
	for i := 0; i < 5; i++ {
		if err := svc.RecordEvent(ctx, tenant, EventObjectCreated, fmt.Sprintf("obj-%d", i), objBytes); err != nil {
			t.Fatalf("record create %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := svc.RecordEvent(ctx, tenant, EventObjectDeleted, fmt.Sprintf("obj-%d", i), objBytes); err != nil {
			t.Fatalf("record delete %d: %v", i, err)
		}
	}

	if got := countStorageEvents(t, pool, tenant); got != 7 {
		t.Fatalf("rows recorded = %d, want 7", got)
	}

	total, err := svc.ReconcileTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("ReconcileTenant: %v", err)
	}
	// 5 creates - 2 deletes = 3 net objects * 100 bytes = 300.
	if want := int64((5 - 2) * objBytes); total != want {
		t.Fatalf("reconciled total = %d, want %d", total, want)
	}
}

// TestStorageEventService_ReconcileEmpty verifies an unknown tenant
// reconciles to zero rather than erroring (COALESCE on the SUM).
func TestStorageEventService_ReconcileEmpty(t *testing.T) {
	pool := testPool(t)
	svc := NewStorageEventService(pool)
	tenant := seedTenant(t, pool, "active")

	total, err := svc.ReconcileTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ReconcileTenant: %v", err)
	}
	if total != 0 {
		t.Fatalf("empty tenant total = %d, want 0", total)
	}
}
