package schemamigrate

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
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

func mig(version int, filename string) Migration {
	return Migration{Version: version, Filename: filename, UpPath: filename}
}

func TestAppliedVersionsParsesFilenamePrefixes(t *testing.T) {
	t.Parallel()
	applied := map[string]bool{
		"001_one.sql":           true,
		"006_feature_flags.sql": true,
		"not_a_migration.sql":   true, // no leading version → ignored
	}
	got := appliedVersions(applied)
	if got[1] != "001_one.sql" {
		t.Errorf("version 1 = %q, want 001_one.sql", got[1])
	}
	if got[6] != "006_feature_flags.sql" {
		t.Errorf("version 6 = %q, want 006_feature_flags.sql", got[6])
	}
	if _, ok := got[0]; ok {
		t.Error("unversioned filename should not appear in appliedVersions")
	}
}

func TestPlanUpAppliesPending(t *testing.T) {
	t.Parallel()
	migs := []Migration{mig(1, "001_one.sql"), mig(2, "002_two.sql")}
	plan := planUp(migs, map[string]bool{"001_one.sql": true})
	if plan[0].Action != actionSkip {
		t.Errorf("001 should be skipped (already applied), got %v", plan[0].Action)
	}
	if plan[1].Action != actionApply {
		t.Errorf("002 should be applied (pending), got %v", plan[1].Action)
	}
}

// TestPlanUpReconcilesRename is the long-term fix for the
// rename-vs-filename-keyed-tracking concern: when a migration file is
// renamed but keeps its version, the runner must recognise it by
// version and reconcile the bookkeeping row rather than re-running an
// already-applied migration.
func TestPlanUpReconcilesRename(t *testing.T) {
	t.Parallel()
	// Version 6 was applied under its old name; the file on disk has
	// since been renamed (same version, new description).
	applied := map[string]bool{"006_old_name.sql": true}
	migs := []Migration{mig(6, "006_new_name.sql")}

	plan := planUp(migs, applied)
	if len(plan) != 1 {
		t.Fatalf("want 1 planned migration, got %d", len(plan))
	}
	if plan[0].Action != actionReconcile {
		t.Fatalf("renamed migration should reconcile, got action %v", plan[0].Action)
	}
	if plan[0].OldFilename != "006_old_name.sql" {
		t.Errorf("OldFilename = %q, want 006_old_name.sql", plan[0].OldFilename)
	}
}

// TestPlanUpRenumberAppliesFresh documents that a *renumber* (the
// version itself changes, e.g. resolving a duplicate-version clash by
// moving 006_x.sql → 007_x.sql) is a different version and therefore a
// genuinely new migration: it is applied, not reconciled. A renumbered
// file is only ever introduced when the old number was never applied
// (Discover hard-errors on a duplicate version), so this is correct.
func TestPlanUpRenumberAppliesFresh(t *testing.T) {
	t.Parallel()
	applied := map[string]bool{"006_feature_flags.sql": true}
	migs := []Migration{
		mig(6, "006_feature_flags.sql"),
		mig(7, "007_confidential_send_mls.sql"),
	}
	plan := planUp(migs, applied)
	if plan[0].Action != actionSkip {
		t.Errorf("006_feature_flags already applied, want skip, got %v", plan[0].Action)
	}
	if plan[1].Action != actionApply {
		t.Errorf("007 is a new version, want apply, got %v", plan[1].Action)
	}
}
