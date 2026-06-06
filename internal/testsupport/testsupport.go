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
	"net/url"
	"os"
	"strings"
	"sync"
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

// rlsRole / rlsPassword name the dedicated non-superuser, NOBYPASSRLS
// login used by RLSPool. The password is a local-only test credential
// (the integration DB is ephemeral), never a production secret.
const (
	rlsRole     = "kmail_test_rls"
	rlsPassword = "kmail_test_rls_pw"
)

var rlsProvisionOnce sync.Once

// RLSPool returns a pool connected as a non-superuser role with
// NOBYPASSRLS set, so Postgres row-level-security policies are actually
// enforced (the default `postgres` superuser bypasses RLS, which would
// let cross-tenant rows leak into RLS-scoped reads like audit
// VerifyChain). The role is provisioned idempotently using the
// superuser pool the first time this is called. If the superuser pool
// lacks permission to create the role, the test is skipped.
//
// The DSN is derived from the superuser DSN by swapping the userinfo,
// so it targets the same host/database.
func RLSPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := Pool(t)
	ctx := context.Background()

	var provErr error
	rlsProvisionOnce.Do(func() { provErr = provisionRLSRole(ctx, admin) })
	if provErr != nil {
		// Re-run grants on subsequent calls in case the once-block ran
		// in another package's binary; surface only hard failures.
		if err := provisionRLSRole(ctx, admin); err != nil {
			t.Skipf("cannot provision RLS test role (%v); skipping RLS-enforced test", err)
		}
	}

	dsn := os.Getenv("KMAIL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	rlsDSN, err := swapUserinfo(dsn, rlsRole, rlsPassword)
	if err != nil {
		t.Skipf("cannot derive RLS DSN (%v); skipping", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(cctx, rlsDSN)
	if err != nil {
		t.Skipf("RLS pool dial failed (%v); skipping", err)
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		t.Skipf("RLS role unreachable (%v); skipping", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func provisionRLSRole(ctx context.Context, admin *pgxpool.Pool) error {
	stmts := []string{
		fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='%s') THEN
				CREATE ROLE %s LOGIN PASSWORD '%s' NOBYPASSRLS;
			END IF;
		END $$;`, rlsRole, rlsRole, rlsPassword),
		// Ensure the password / attributes match even if a previous
		// run created the role with different settings.
		fmt.Sprintf(`ALTER ROLE %s LOGIN NOBYPASSRLS PASSWORD '%s'`, rlsRole, rlsPassword),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, rlsRole),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s`, rlsRole),
		fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s`, rlsRole),
	}
	for _, s := range stmts {
		if _, err := admin.Exec(ctx, s); err != nil {
			// Ignore the benign race where a concurrent test binary
			// created the role between our existence check and CREATE.
			if strings.Contains(err.Error(), "already exists") {
				continue
			}
			return err
		}
	}
	return nil
}

func swapUserinfo(dsn, user, pass string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, pass)
	return u.String(), nil
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
