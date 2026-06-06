package audit

// Audit hash-chain integrity under concurrent writes (Session 6 /
// SOC 2 data-protection step). Skips unless KMAIL_TEST_DATABASE_URL
// or DATABASE_URL points at a migrated database.
//
// Before migration 008/009 + the per-tenant advisory lock in
// Service.Log, concurrent appends to one tenant's chain raced on
// the read-latest-hash → insert sequence and forked the chain
// (two rows sharing a prev_hash), which VerifyChain then reports as
// broken. This test hammers a single tenant with concurrent Log
// calls and asserts the chain stays linear and verifiable.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KMAIL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set KMAIL_TEST_DATABASE_URL or DATABASE_URL to run audit-chain DB tests")
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

func seedTenant(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	slug := fmt.Sprintf("audit-chain-test-%d", time.Now().UnixNano())
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tenants (name, slug, plan, status)
		VALUES ('audit-chain-test', $1, 'pro', 'active')
		RETURNING id::text
	`, slug).Scan(&id); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, id)
	})
	return id
}

func TestAuditChain_ConcurrentWritesStayLinear(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)
	svc := NewService(pool)
	ctx := context.Background()

	const writers = 16
	const perWriter = 8
	var wg sync.WaitGroup
	errCh := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_, err := svc.Log(ctx, Entry{
					TenantID:     tenantID,
					ActorID:      fmt.Sprintf("actor-%d", w),
					ActorType:    ActorSystem,
					Action:       "test.concurrent.append",
					ResourceType: "test",
					ResourceID:   fmt.Sprintf("%d-%d", w, i),
				})
				if err != nil {
					errCh <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent Log failed: %v", err)
	}

	// The chain must verify end to end (no forks, no broken links).
	if err := svc.VerifyChain(ctx, tenantID); err != nil {
		t.Fatalf("VerifyChain after concurrent writes: %v", err)
	}

	// Sanity: exactly one genesis row (prev_hash = '') exists for the
	// tenant — the structural invariant enforced by migration 009.
	var genesis int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1::uuid AND prev_hash = ''`, tenantID,
	).Scan(&genesis); err != nil {
		t.Fatalf("count genesis rows: %v", err)
	}
	if genesis != 1 {
		t.Errorf("expected exactly 1 genesis row, got %d (chain forked)", genesis)
	}

	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1::uuid`, tenantID,
	).Scan(&total); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if total != writers*perWriter {
		t.Errorf("expected %d audit rows, got %d", writers*perWriter, total)
	}
}
