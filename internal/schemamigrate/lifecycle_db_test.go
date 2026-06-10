package schemamigrate

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
)

// TestUpStatusDownLifecycle drives a synthetic migration set through
// the full runner lifecycle: Up applies pending files, Status reports
// applied/has-down, and Down rolls the most recent one back.
func TestUpStatusDownLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Start from a clean bookkeeping table inside the isolated schema.
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS schema_migrations"); err != nil {
		t.Fatalf("reset: %v", err)
	}

	dir := t.TempDir()
	// 001: a table with a rollback companion.
	writeFile(t, dir, "001_widgets.sql", "CREATE TABLE sm_widgets (id INT PRIMARY KEY);")
	writeFile(t, dir, "001_widgets.down.sql", "DROP TABLE sm_widgets;")
	// 002: a table WITHOUT a rollback companion.
	writeFile(t, dir, "002_gadgets.sql", "CREATE TABLE sm_gadgets (id INT PRIMARY KEY);")

	r := NewRunner(pool, dir, log.New(io.Discard, "", 0))

	if err := r.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Status: both applied; 001 has a down file, 002 does not.
	rows, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("status rows = %d, want 2", len(rows))
	}
	byVer := map[int]StatusRow{}
	for _, sr := range rows {
		byVer[sr.Version] = sr
	}
	if !byVer[1].Applied || !byVer[1].HasDown {
		t.Errorf("001 status = %+v; want applied+hasDown", byVer[1])
	}
	if !byVer[2].Applied || byVer[2].HasDown {
		t.Errorf("002 status = %+v; want applied, no down", byVer[2])
	}

	// Down 1 step: 002 is newest but has no down file → ErrNoDownFile,
	// and nothing must be mutated.
	if err := r.Down(ctx, 1); !errors.Is(err, ErrNoDownFile) {
		t.Fatalf("Down(1) on no-down migration: want ErrNoDownFile got %v", err)
	}

	// Remove 002 so the newest rollback target (001) has a down file.
	if _, err := pool.Exec(ctx, "DROP TABLE sm_gadgets"); err != nil {
		t.Fatalf("drop gadgets: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM schema_migrations WHERE filename = '002_gadgets.sql'"); err != nil {
		t.Fatalf("unrecord 002: %v", err)
	}

	// Down 1 step now rolls back 001.
	if err := r.Down(ctx, 1); err != nil {
		t.Fatalf("Down(1): %v", err)
	}
	var widgetCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM information_schema.tables WHERE table_name = 'sm_widgets' AND table_schema = current_schema()").Scan(&widgetCount); err != nil {
		t.Fatalf("count widgets table: %v", err)
	}
	if widgetCount != 0 {
		t.Errorf("sm_widgets should have been dropped by Down, still present")
	}
	var remaining int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Errorf("schema_migrations should be empty after rollback, got %d", remaining)
	}
}

// TestDownStepsValidation pins the positive-steps guard.
func TestDownStepsValidation(t *testing.T) {
	pool := testPool(t)
	r := NewRunner(pool, t.TempDir(), log.New(io.Discard, "", 0))
	if err := r.Down(context.Background(), 0); err == nil {
		t.Error("Down(0) should error")
	}
	if err := r.Down(context.Background(), -3); err == nil {
		t.Error("Down(-3) should error")
	}
}

// TestDownMoreStepsThanApplied clamps steps to the number of applied
// migrations and is a no-op when nothing is applied.
func TestDownMoreStepsThanApplied(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS schema_migrations"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	r := NewRunner(pool, t.TempDir(), log.New(io.Discard, "", 0))
	// Nothing discovered, nothing applied: Down clamps steps to 0 and
	// returns cleanly.
	if err := r.Down(ctx, 5); err != nil {
		t.Fatalf("Down on empty: %v", err)
	}
}
