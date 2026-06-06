// Package testsupport provides shared helpers for Go integration
// tests that need a Postgres pool and seeded tenant rows.
//
// The helpers follow the existing repo convention (see
// internal/billing/storage_events_test.go): a test that needs the
// database is SKIPPED when neither KMAIL_TEST_DATABASE_URL nor
// DATABASE_URL is set, so `make test` / CI (which have no Postgres)
// stay infra-free. When a DSN is set the pool is dialled, pinged,
// and registered for cleanup.
//
// This package imports "testing" and is therefore only ever linked
// into test binaries — it is never imported by production code.
package testsupport

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool dials the integration database named by KMAIL_TEST_DATABASE_URL
// (or DATABASE_URL). When neither is set the calling test is skipped.
// The returned pool is closed via t.Cleanup.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KMAIL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set KMAIL_TEST_DATABASE_URL or DATABASE_URL to run DB integration tests")
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

// SeedTenant inserts a tenant with the given plan and status and
// registers cleanup that removes it (cascading child rows). It
// returns the new tenant UUID as a string.
func SeedTenant(t *testing.T, pool *pgxpool.Pool, plan, status string) string {
	t.Helper()
	if plan == "" {
		plan = "pro"
	}
	if status == "" {
		status = "active"
	}
	ctx := context.Background()
	n := atomic.AddInt64(&tenantSeq, 1)
	slug := fmt.Sprintf("ts-%d-%d", time.Now().UnixNano(), n)
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO tenants (name, slug, plan, status)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text
	`, "testsupport-tenant", slug, plan, status).Scan(&id)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, id)
	})
	return id
}
