package featureflags

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func randSuffix() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

// TestStoreCRUDLifecycle drives the Store write+read paths against the
// live control-plane tables: upsert a flag, set/override, load views,
// then delete both.
func TestStoreCRUDLifecycle(t *testing.T) {
	pool := testsupport.Pool(t)
	ctx := context.Background()
	s := NewStore(pool)

	key := "ff_test_" + randSuffix()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM feature_flags WHERE key = $1", key)
	})

	// UpsertFlag (insert).
	f, err := s.UpsertFlag(ctx, Flag{Key: key, Description: "test flag", DefaultEnabled: false})
	if err != nil {
		t.Fatalf("UpsertFlag insert: %v", err)
	}
	if f.Key != key || f.DefaultEnabled {
		t.Fatalf("unexpected flag: %+v", f)
	}

	// UpsertFlag (update / on-conflict).
	f, err = s.UpsertFlag(ctx, Flag{Key: key, Description: "updated", DefaultEnabled: true})
	if err != nil {
		t.Fatalf("UpsertFlag update: %v", err)
	}
	if !f.DefaultEnabled || f.Description != "updated" {
		t.Fatalf("update not applied: %+v", f)
	}

	// SetOverride (global).
	ov, err := s.SetOverride(ctx, Override{FlagKey: key, Scope: ScopeGlobal, Enabled: true})
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if ov.Scope != ScopeGlobal || !ov.Enabled {
		t.Fatalf("unexpected override: %+v", ov)
	}

	// SetOverride conflict update path (toggle enabled).
	ov, err = s.SetOverride(ctx, Override{FlagKey: key, Scope: ScopeGlobal, Enabled: false})
	if err != nil {
		t.Fatalf("SetOverride update: %v", err)
	}
	if ov.Enabled {
		t.Fatalf("override should be disabled after update: %+v", ov)
	}

	// loadViews must surface the flag with its override nested.
	views, err := s.loadViews(ctx)
	if err != nil {
		t.Fatalf("loadViews: %v", err)
	}
	var found *FlagView
	for i := range views {
		if views[i].Key == key {
			found = &views[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("flag %s not in views", key)
	}
	if len(found.Overrides) != 1 {
		t.Fatalf("want 1 override, got %d", len(found.Overrides))
	}

	// DeleteOverride then DeleteFlag.
	if err := s.DeleteOverride(ctx, key, ScopeGlobal, ""); err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
	if err := s.DeleteFlag(ctx, key); err != nil {
		t.Fatalf("DeleteFlag: %v", err)
	}
}

// TestStoreTenantPlan resolves a seeded tenant's plan and returns
// empty for an unknown tenant.
func TestStoreTenantPlan(t *testing.T) {
	pool := testsupport.Pool(t)
	ctx := context.Background()
	s := NewStore(pool)

	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	plan, err := s.tenantPlan(ctx, tenant)
	if err != nil {
		t.Fatalf("tenantPlan: %v", err)
	}
	if plan != "pro" {
		t.Errorf("plan = %q want pro", plan)
	}

	// Unknown (well-formed) tenant id → ("", nil).
	plan, err = s.tenantPlan(ctx, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("tenantPlan unknown: %v", err)
	}
	if plan != "" {
		t.Errorf("unknown tenant plan = %q want empty", plan)
	}
}

// TestStoreUpsertFlagValidation guards the empty-key reject.
func TestStoreUpsertFlagValidation(t *testing.T) {
	pool := testsupport.Pool(t)
	if _, err := NewStore(pool).UpsertFlag(context.Background(), Flag{Key: ""}); err == nil {
		t.Error("empty key should error")
	}
}
