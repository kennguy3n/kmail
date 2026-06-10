package tenant

import (
	"context"
	"errors"
	"testing"
	"time"
)

// countingProvisioner / countingBilling record how many times the
// idempotent post-insert hooks run so the EnsureProvisioned gating
// can be asserted without standing up zk-object-fabric or billing.
type countingProvisioner struct {
	calls int
	err   error
}

func (c *countingProvisioner) Provision(ctx context.Context, tenantID, plan string) (*StorageCredential, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return &StorageCredential{TenantID: tenantID}, nil
}

type countingBilling struct {
	created int
	err     error
}

func (c *countingBilling) OnTenantCreated(ctx context.Context, tenantID, plan string) error {
	c.created++
	return c.err
}

func (c *countingBilling) OnTenantDeleted(ctx context.Context, tenantID string) error { return nil }

// TestReconcileExistingHookGating verifies the partial-provisioning
// fix: on the existing-row path the idempotent post-insert hooks are
// re-run only when the caller sets EnsureProvisioned (the webhook),
// not on the id-only lazy hot path. The input metadata matches the
// existing row so reconcileTenantMetadata is a no-op and no DB pool
// is required.
func TestReconcileExistingHookGating(t *testing.T) {
	existing := &Tenant{ID: "11111111-1111-1111-1111-111111111111", Name: "Acme", Slug: "acme", Plan: "pro", CreatedAt: time.Now(), UpdatedAt: time.Now()}

	t.Run("lazy hot path does not re-run hooks", func(t *testing.T) {
		prov := &countingProvisioner{}
		bill := &countingBilling{}
		svc := (&Service{}).WithStorageProvisioner(prov).WithBillingLifecycle(bill)

		_, created, err := svc.reconcileExisting(context.Background(), existing, EnsureTenantInput{ID: existing.ID})
		if err != nil {
			t.Fatalf("reconcileExisting: %v", err)
		}
		if created {
			t.Error("created = true on existing row, want false")
		}
		if prov.calls != 0 || bill.created != 0 {
			t.Errorf("hooks re-run on lazy path: provision=%d billing=%d, want 0/0", prov.calls, bill.created)
		}
	})

	t.Run("webhook path re-runs idempotent hooks", func(t *testing.T) {
		prov := &countingProvisioner{}
		bill := &countingBilling{}
		svc := (&Service{}).WithStorageProvisioner(prov).WithBillingLifecycle(bill)

		_, created, err := svc.reconcileExisting(context.Background(), existing, EnsureTenantInput{ID: existing.ID, Name: existing.Name, Slug: existing.Slug, Plan: existing.Plan, EnsureProvisioned: true})
		if err != nil {
			t.Fatalf("reconcileExisting: %v", err)
		}
		if created {
			t.Error("created = true on existing row, want false")
		}
		if prov.calls != 1 || bill.created != 1 {
			t.Errorf("hooks not re-run on webhook path: provision=%d billing=%d, want 1/1", prov.calls, bill.created)
		}
	})

	t.Run("hook failure surfaces for caller retry", func(t *testing.T) {
		wantErr := errors.New("bucket create failed")
		prov := &countingProvisioner{err: wantErr}
		svc := (&Service{}).WithStorageProvisioner(prov)

		_, _, err := svc.reconcileExisting(context.Background(), existing, EnsureTenantInput{ID: existing.ID, EnsureProvisioned: true})
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want wrap of %v", err, wantErr)
		}
	})
}
