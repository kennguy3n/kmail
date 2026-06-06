package schemamigrate

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// testPool dials the integration database named by
// KMAIL_TEST_DATABASE_URL (or DATABASE_URL). When neither is set — the
// default for `make test` and CI, which have no Postgres — the calling
// test is skipped.
//
// The pool is pinned to a freshly-created, uniquely-named schema via
// search_path and that schema is dropped on cleanup, so these tests
// (which create/drop schema_migrations and confidential_send_links) are
// hermetic and never touch the shared public schema of a dev database.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KMAIL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set KMAIL_TEST_DATABASE_URL or DATABASE_URL to run schemamigrate DB tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Skipf("parse DSN (%v); skipping integration test", err)
	}
	schema := fmt.Sprintf("schemamigrate_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+quoted)
		return err
	}

	// Create the schema with a throwaway connection that uses the
	// default search_path (the AfterConnect hook would otherwise SET a
	// path to a schema that does not exist yet).
	bootCfg := cfg.Copy()
	bootCfg.AfterConnect = nil
	boot, err := pgxpool.NewWithConfig(ctx, bootCfg)
	if err != nil {
		t.Skipf("database unreachable (%v); skipping integration test", err)
	}
	if err := boot.Ping(ctx); err != nil {
		boot.Close()
		t.Skipf("database unreachable (%v); skipping integration test", err)
	}
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		boot.Close()
		t.Fatalf("create test schema: %v", err)
	}
	boot.Close()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(dropCtx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Logf("drop test schema %s: %v", schema, err)
		}
		pool.Close()
	})
	return pool
}

// TestUpReconcilesRenumbered006To007 is the integration regression for
// the 006 → 007 renumber. A database that applied the migration under
// its old filename (006_confidential_send_mls.sql, while ws5 was the
// sole version 6 on main) must, after this PR, end up with a single
// 007_confidential_send_mls.sql bookkeeping row — the renamed file must
// NOT be re-applied and must NOT leave an orphaned 006 row. The
// reconciliation lives in the committed 007 SQL, so this test runs the
// real file through the Runner against Postgres.
func TestUpReconcilesRenumbered006To007(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Clean slate, then reconstruct the "window" state: the migration
	// was applied under its old 006 name and confidential_send_links
	// already exists (created by an earlier migration in the real tree).
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS schema_migrations",
		"DROP TABLE IF EXISTS confidential_send_links",
		"CREATE TABLE confidential_send_links (id TEXT PRIMARY KEY)",
		"CREATE TABLE schema_migrations (filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())",
		"INSERT INTO schema_migrations (filename) VALUES ('006_confidential_send_mls.sql')",
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	// A dir containing only the real, committed 007 file.
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", "migrations", "007_confidential_send_mls.sql"))
	if err != nil {
		t.Fatalf("read 007 migration: %v", err)
	}
	writeFile(t, dir, "007_confidential_send_mls.sql", string(src))

	r := NewRunner(pool, dir, log.New(io.Discard, "", 0))
	// Running twice must be idempotent.
	for i := 0; i < 2; i++ {
		if err := r.Up(ctx); err != nil {
			t.Fatalf("Up (run %d): %v", i+1, err)
		}
	}

	var newRows, oldRows int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM schema_migrations WHERE filename = '007_confidential_send_mls.sql'").Scan(&newRows); err != nil {
		t.Fatalf("count 007 rows: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM schema_migrations WHERE filename = '006_confidential_send_mls.sql'").Scan(&oldRows); err != nil {
		t.Fatalf("count 006 rows: %v", err)
	}
	if newRows != 1 {
		t.Errorf("want exactly one 007_confidential_send_mls.sql row, got %d", newRows)
	}
	if oldRows != 0 {
		t.Errorf("orphaned 006_confidential_send_mls.sql row should be gone, got %d", oldRows)
	}
}

// TestUpAppliesRenumberedFreshInstall pins the no-op path: on a fresh
// database (no prior 006 bookkeeping), the reconciliation statements do
// nothing and 007 is recorded normally.
func TestUpAppliesRenumberedFreshInstall(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	for _, stmt := range []string{
		"DROP TABLE IF EXISTS schema_migrations",
		"DROP TABLE IF EXISTS confidential_send_links",
		"CREATE TABLE confidential_send_links (id TEXT PRIMARY KEY)",
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", "migrations", "007_confidential_send_mls.sql"))
	if err != nil {
		t.Fatalf("read 007 migration: %v", err)
	}
	writeFile(t, dir, "007_confidential_send_mls.sql", string(src))

	r := NewRunner(pool, dir, log.New(io.Discard, "", 0))
	if err := r.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	var newRows int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM schema_migrations WHERE filename = '007_confidential_send_mls.sql'").Scan(&newRows); err != nil {
		t.Fatalf("count 007 rows: %v", err)
	}
	if newRows != 1 {
		t.Errorf("fresh install should record exactly one 007 row, got %d", newRows)
	}
}

func TestDiscoverOrdersAndPairsDownFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "002_two.sql", "SELECT 2;")
	writeFile(t, dir, "001_one.sql", "SELECT 1;")
	writeFile(t, dir, "001_one.down.sql", "SELECT -1;")
	writeFile(t, dir, "010_ten.sql", "SELECT 10;")
	// noise that must be ignored
	writeFile(t, dir, "README.md", "nope")
	writeFile(t, dir, "notes.txt", "nope")

	migs, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(migs) != 3 {
		t.Fatalf("want 3 migrations, got %d: %+v", len(migs), migs)
	}
	wantOrder := []int{1, 2, 10}
	for i, w := range wantOrder {
		if migs[i].Version != w {
			t.Fatalf("order[%d] = %d, want %d", i, migs[i].Version, w)
		}
	}
	if migs[0].DownPath == "" {
		t.Error("001 should have a down file")
	}
	if migs[1].DownPath != "" {
		t.Error("002 should NOT have a down file")
	}
}

func TestDiscoverDuplicateVersionErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "003_a.sql", "SELECT 1;")
	writeFile(t, dir, "003_b.sql", "SELECT 2;")
	if _, err := Discover(dir); err == nil {
		t.Fatal("expected duplicate-version error")
	}
}

func TestDiscoverEmptyDir(t *testing.T) {
	t.Parallel()
	migs, err := Discover(t.TempDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(migs) != 0 {
		t.Fatalf("want 0, got %d", len(migs))
	}
}

func TestDiscoverRealMigrationsDir(t *testing.T) {
	t.Parallel()
	// The repo's actual migrations dir must be discoverable and
	// strictly increasing — guards against a malformed new migration
	// filename landing in the tree.
	migs, err := Discover(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("Discover real migrations: %v", err)
	}
	if len(migs) < 6 {
		t.Fatalf("expected at least 6 migrations, got %d", len(migs))
	}
	for i := 1; i < len(migs); i++ {
		if migs[i].Version <= migs[i-1].Version {
			t.Fatalf("migrations not strictly increasing at %d: %d <= %d", i, migs[i].Version, migs[i-1].Version)
		}
	}
}
