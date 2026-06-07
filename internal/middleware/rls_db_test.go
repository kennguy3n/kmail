package middleware

// Cross-tenant isolation integration tests (Session 6 / SOC 2
// data-protection step). These exercise PostgreSQL row-level
// security against a real database:
//
//   - TestRLS_ForcedOnEveryEnabledTable is a schema invariant:
//     every table that ENABLEs RLS must also FORCE it, so the
//     policies bind even when the application connects as the
//     table owner. This directly guards migration 010.
//
//   - TestRLS_CrossTenantIsolation connects as a non-superuser,
//     non-BYPASSRLS role and asserts that, with the tenant GUC set
//     to tenant A, every attempt to read / mutate tenant B's rows
//     fails (zero rows or a WITH CHECK violation). It also runs
//     concurrent readers for different tenants to confirm
//     isolation holds under load.
//
// All tests skip when neither KMAIL_TEST_DATABASE_URL nor
// DATABASE_URL is set (the default for `make test` / CI, which have
// no Postgres). The target database must already have migrations
// applied.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("KMAIL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set KMAIL_TEST_DATABASE_URL or DATABASE_URL to run RLS isolation tests")
	}
	return dsn
}

func testAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testDSN(t)
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

// seedTenantWithUser inserts an active tenant plus one user and
// registers cleanup that removes the tenant (cascading to the
// user). Returns (tenantID, userID).
func seedTenantWithUser(t *testing.T, pool *pgxpool.Pool, slugPrefix string) (string, string) {
	t.Helper()
	ctx := context.Background()
	slug := fmt.Sprintf("%s-%d", slugPrefix, time.Now().UnixNano())
	var tenantID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tenants (name, slug, plan, status)
		VALUES ($1, $2, 'pro', 'active')
		RETURNING id::text
	`, slugPrefix, slug).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, tenantID)
	})
	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (tenant_id, kchat_user_id, stalwart_account_id, email, display_name, status)
		VALUES ($1::uuid, $2, $3, $4, $5, 'active')
		RETURNING id::text
	`, tenantID, slug+"-kc", slug+"-sa", slug+"@example.test", slugPrefix).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return tenantID, userID
}

// TestRLS_ForcedOnEveryEnabledTable asserts the schema invariant
// that underpins tenant isolation: a table that enables RLS but
// does not force it leaves the policy unenforced for the table
// owner. Migration 010 forces RLS on every such table; this test
// fails if a future migration enables RLS without forcing it.
func TestRLS_ForcedOnEveryEnabledTable(t *testing.T) {
	pool := testAdminPool(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND c.relrowsecurity = true
		  AND c.relforcerowsecurity = false
		ORDER BY c.relname
	`)
	if err != nil {
		t.Fatalf("query pg_class: %v", err)
	}
	defer rows.Close()
	var offenders []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		offenders = append(offenders, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("tables ENABLE RLS but do not FORCE it (owner bypasses isolation): %v", offenders)
	}
}

// rlsTestRole creates a dedicated NOSUPERUSER / NOBYPASSRLS login
// role with table privileges and returns a pool connected as that
// role. RLS policies are evaluated for this role (unlike the
// superuser/owner the migrations run as), so it is the realistic
// stand-in for the application's runtime database role.
func rlsTestRole(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	const role = "kmail_rls_test"
	const pw = "kmail_rls_test_pw"
	// Recreate the role cleanly. DROP OWNED first in case a prior
	// run left grants behind.
	_, _ = admin.Exec(ctx, `DROP OWNED BY `+role)
	_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS `+role)
	if _, err := admin.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE %s LOGIN PASSWORD %s NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
		role, quoteLiteral(pw),
	)); err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, stmt := range []string{
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("grant (%s): %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.Exec(c, `DROP OWNED BY `+role)
		_, _ = admin.Exec(c, `DROP ROLE IF EXISTS `+role)
	})

	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = pw
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRLS_CrossTenantIsolation(t *testing.T) {
	admin := testAdminPool(t)
	tenantA, _ := seedTenantWithUser(t, admin, "rls-a")
	tenantB, userB := seedTenantWithUser(t, admin, "rls-b")
	appPool := rlsTestRole(t, admin)
	ctx := context.Background()

	// With the GUC pinned to tenant A, tenant B's user row must be
	// invisible and immutable. (The non-erroring reads/mutations run
	// in one transaction; the cross-tenant INSERT runs in its own
	// because a WITH CHECK violation aborts the surrounding tx.)
	err := pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantA); err != nil {
			return err
		}
		var visible int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM users WHERE id = $1::uuid`, userB,
		).Scan(&visible); err != nil {
			return fmt.Errorf("select B as A: %w", err)
		}
		if visible != 0 {
			t.Errorf("tenant A can see tenant B's user row (count=%d); RLS leak", visible)
		}

		tag, err := tx.Exec(ctx,
			`UPDATE users SET display_name = 'pwned' WHERE id = $1::uuid`, userB)
		if err != nil {
			return fmt.Errorf("update B as A: %w", err)
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("tenant A updated %d of tenant B's rows; RLS leak", tag.RowsAffected())
		}

		tag, err = tx.Exec(ctx,
			`DELETE FROM users WHERE id = $1::uuid`, userB)
		if err != nil {
			return fmt.Errorf("delete B as A: %w", err)
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("tenant A deleted %d of tenant B's rows; RLS leak", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("isolation tx: %v", err)
	}

	// Cross-tenant INSERT: claim tenant B's id while the GUC says A.
	// The policy's WITH CHECK must reject it (the tx then aborts).
	insErr := pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantA); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO users (tenant_id, kchat_user_id, stalwart_account_id, email, display_name, status)
			VALUES ($1::uuid, 'evil-kc', 'evil-sa', 'evil@example.test', 'evil', 'active')
		`, tenantB)
		return err
	})
	if insErr == nil {
		t.Error("tenant A inserted a row for tenant B; WITH CHECK not enforced")
	}

	// Confirm tenant B's row still exists and is unmodified (seen
	// from B's own scope), proving the attacker's writes were no-ops.
	if err := pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantB); err != nil {
			return err
		}
		var name string
		if err := tx.QueryRow(ctx,
			`SELECT display_name FROM users WHERE id = $1::uuid`, userB,
		).Scan(&name); err != nil {
			return err
		}
		if name == "pwned" {
			t.Error("tenant B's row was modified across tenants")
		}
		return nil
	}); err != nil {
		t.Fatalf("verify B tx: %v", err)
	}
}

// TestRLS_ConcurrentTenantsStayIsolated drives concurrent readers
// for two tenants through the restricted role and asserts each only
// ever sees its own row, even under contention.
func TestRLS_ConcurrentTenantsStayIsolated(t *testing.T) {
	admin := testAdminPool(t)
	tenantA, userA := seedTenantWithUser(t, admin, "rls-conc-a")
	tenantB, userB := seedTenantWithUser(t, admin, "rls-conc-b")
	appPool := rlsTestRole(t, admin)

	type probe struct {
		tenant   string
		ownUser  string
		otherUsr string
	}
	probes := []probe{
		{tenantA, userA, userB},
		{tenantB, userB, userA},
	}

	const iterations = 50
	var wg sync.WaitGroup
	errCh := make(chan error, len(probes)*iterations)
	for _, p := range probes {
		for i := 0; i < iterations; i++ {
			wg.Add(1)
			go func(p probe) {
				defer wg.Done()
				ctx := context.Background()
				err := pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
					if err := SetTenantGUC(ctx, tx, p.tenant); err != nil {
						return err
					}
					var own, other int
					if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1::uuid`, p.ownUser).Scan(&own); err != nil {
						return err
					}
					if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1::uuid`, p.otherUsr).Scan(&other); err != nil {
						return err
					}
					if own != 1 {
						return fmt.Errorf("tenant %s could not see its own row (count=%d)", p.tenant, own)
					}
					if other != 0 {
						return fmt.Errorf("tenant %s saw another tenant's row (count=%d)", p.tenant, other)
					}
					return nil
				})
				if err != nil {
					errCh <- err
				}
			}(p)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// quoteLiteral wraps a string in single quotes for use as a SQL
// string literal, doubling embedded quotes. Used only for the test
// role password (a constant), so it does not need to handle the
// full set of escaping edge cases.
func quoteLiteral(s string) string {
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += "''"
			continue
		}
		out += string(r)
	}
	return out + "'"
}
