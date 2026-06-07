package retention

import (
	"context"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func retentionService(t *testing.T) (*Service, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	return NewService(pool), tenant
}

func TestRetentionPolicyValidation(t *testing.T) {
	svc := NewService(nil)
	ctx := context.Background()
	bad := []Policy{
		{},                                                            // no tenant
		{TenantID: "t", PolicyType: "nope", RetentionDays: 5, AppliesTo: "all"},
		{TenantID: "t", PolicyType: "delete", RetentionDays: 0, AppliesTo: "all"},
		{TenantID: "t", PolicyType: "delete", RetentionDays: 5, AppliesTo: "weird"},
	}
	for i, p := range bad {
		if _, err := svc.CreatePolicy(ctx, p); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestRetentionPolicyCRUDDB(t *testing.T) {
	svc, tenant := retentionService(t)
	ctx := context.Background()

	p, err := svc.CreatePolicy(ctx, Policy{
		TenantID: tenant, PolicyType: "delete", RetentionDays: 30,
		AppliesTo: "all", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	t.Cleanup(func() { _ = svc.DeletePolicy(context.Background(), tenant, p.ID) })

	disabled, err := svc.CreatePolicy(ctx, Policy{
		TenantID: tenant, PolicyType: "archive", RetentionDays: 90,
		AppliesTo: "label", TargetRef: "Archive", Enabled: false,
	})
	if err != nil {
		t.Fatalf("CreatePolicy disabled: %v", err)
	}
	t.Cleanup(func() { _ = svc.DeletePolicy(context.Background(), tenant, disabled.ID) })

	// Update: flip retention + enable.
	p.RetentionDays = 60
	if _, err := svc.UpdatePolicy(ctx, *p); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}

	list, err := svc.ListPolicies(ctx, tenant)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	var seenUpdated bool
	for _, x := range list {
		if x.ID == p.ID {
			seenUpdated = true
			if x.RetentionDays != 60 {
				t.Errorf("update not persisted: %d", x.RetentionDays)
			}
		}
	}
	if !seenUpdated {
		t.Error("ListPolicies missing created policy")
	}

	// EvaluateRetention with no enforcer counts enabled policies (>=1 here).
	n, err := svc.EvaluateRetention(ctx, tenant)
	if err != nil {
		t.Fatalf("EvaluateRetention: %v", err)
	}
	if n < 1 {
		t.Errorf("EvaluateRetention enabled count=%d want >=1", n)
	}

	// ListActiveTenants includes our active tenant.
	active, err := svc.ListActiveTenants(ctx)
	if err != nil {
		t.Fatalf("ListActiveTenants: %v", err)
	}
	found := false
	for _, id := range active {
		if id == tenant {
			found = true
		}
	}
	if !found {
		t.Error("ListActiveTenants missing seeded active tenant")
	}

	// RecentEnforcementRuns (no rows yet, but exercises the query + limit clamp).
	if _, err := svc.RecentEnforcementRuns(ctx, tenant, 0); err != nil {
		t.Fatalf("RecentEnforcementRuns: %v", err)
	}

	if err := svc.DeletePolicy(ctx, tenant, p.ID); err != nil {
		t.Fatalf("DeletePolicy: %v", err)
	}
}
