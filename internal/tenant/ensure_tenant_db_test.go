package tenant

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestEnsureTenantDB exercises the idempotent, id-explicit
// provisioning path the iam-core webhook + lazy-provision flows
// depend on. Skipped without a test database.
func TestEnsureTenantDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	ctx := context.Background()
	id := uuid.NewString()

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id=$1::uuid`, id)
	})

	// First call provisions the row.
	tn, created, err := svc.EnsureTenant(ctx, EnsureTenantInput{ID: id, Name: "Acme", Slug: "acme-" + id, Plan: "pro"})
	if err != nil {
		t.Fatalf("EnsureTenant (create): %v", err)
	}
	if !created {
		t.Fatal("created = false, want true on first provision")
	}
	if tn.ID != id || tn.Plan != "pro" {
		t.Errorf("unexpected tenant: %+v", tn)
	}

	// An id-only call (the lazy-provisioning hot path) is a pure
	// no-op: it must not mutate the authoritative metadata an
	// earlier webhook persisted.
	again, created2, err := svc.EnsureTenant(ctx, EnsureTenantInput{ID: id})
	if err != nil {
		t.Fatalf("EnsureTenant (id-only no-op): %v", err)
	}
	if created2 {
		t.Error("created = true on id-only call, want false (idempotent)")
	}
	if again.ID != id || again.Slug != tn.Slug || again.Name != tn.Name || again.Plan != tn.Plan {
		t.Errorf("id-only call mutated the row: %+v vs %+v", again, tn)
	}

	// A subsequent authoritative call (e.g. a tenant.create webhook
	// arriving after a placeholder lazy provision) reconciles the
	// changed metadata onto the existing row while still reporting
	// created=false.
	newSlug := "renamed-" + id
	reconciled, created3, err := svc.EnsureTenant(ctx, EnsureTenantInput{ID: id, Name: "Acme Renamed", Slug: newSlug, Plan: "privacy"})
	if err != nil {
		t.Fatalf("EnsureTenant (reconcile): %v", err)
	}
	if created3 {
		t.Error("created = true on reconcile call, want false")
	}
	if reconciled.Name != "Acme Renamed" || reconciled.Slug != newSlug || reconciled.Plan != "privacy" {
		t.Errorf("reconcile did not update metadata: %+v", reconciled)
	}
}

func TestEnsureTenantDefaultsAndValidation(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	ctx := context.Background()

	// Invalid UUID is rejected before touching the DB.
	if _, _, err := svc.EnsureTenant(ctx, EnsureTenantInput{ID: "not-a-uuid"}); err == nil {
		t.Fatal("expected error for non-UUID id")
	}

	// Empty Name/Slug/Plan are defaulted (Name/Slug from ID, Plan to
	// "core") so a lazy provision that only knows the id still
	// satisfies the NOT NULL + CHECK columns.
	id := uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id=$1::uuid`, id)
	})
	tn, created, err := svc.EnsureTenant(ctx, EnsureTenantInput{ID: id})
	if err != nil {
		t.Fatalf("EnsureTenant (defaults): %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if tn.Slug != id || tn.Name != id || tn.Plan != "core" {
		t.Errorf("defaults not applied: %+v", tn)
	}
}
