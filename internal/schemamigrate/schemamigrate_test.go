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
