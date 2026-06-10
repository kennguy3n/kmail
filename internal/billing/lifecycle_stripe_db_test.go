package billing

import (
	"context"
	"testing"
)

// TestLifecycleStripePathsDB drives OnTenantCreated → OnPlanChanged →
// OnTenantDeleted with a configured (mock) Stripe client so the
// Stripe REST branches in lifecycle.go are exercised end to end and
// the resulting billing_subscriptions row reflects the Stripe IDs.
func TestLifecycleStripePathsDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()
	stripe, _, calls := newMockStripe(t)
	lc := NewLifecycle(svc, nil).WithStripe(stripe, map[string]string{
		PlanPro:  "price_pro",
		PlanCore: "price_core",
	})

	if err := lc.OnTenantCreated(ctx, tenant, PlanPro); err != nil {
		t.Fatalf("OnTenantCreated: %v", err)
	}
	sub, err := lc.GetSubscription(ctx, tenant)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if sub.StripeSubscriptionID != "sub_test" {
		t.Errorf("stripe sub id=%q want sub_test", sub.StripeSubscriptionID)
	}
	if sub.Plan != PlanPro {
		t.Errorf("plan=%q want %s", sub.Plan, PlanPro)
	}

	// Plan change drives a Stripe metadata update (sub has a stripe id
	// and PlanCore has a configured price).
	if err := lc.OnPlanChanged(ctx, tenant, PlanPro, PlanCore); err != nil {
		t.Fatalf("OnPlanChanged: %v", err)
	}
	sub, err = lc.GetSubscription(ctx, tenant)
	if err != nil {
		t.Fatalf("GetSubscription after plan change: %v", err)
	}
	if sub.Plan != PlanCore {
		t.Errorf("plan after change=%q want %s", sub.Plan, PlanCore)
	}

	// Deletion cancels in Stripe and marks the row cancelled.
	if err := lc.OnTenantDeleted(ctx, tenant); err != nil {
		t.Fatalf("OnTenantDeleted: %v", err)
	}
	sub, err = lc.GetSubscription(ctx, tenant)
	if err != nil {
		t.Fatalf("GetSubscription after delete: %v", err)
	}
	if sub.Status != SubscriptionCancelled {
		t.Errorf("status after delete=%s want cancelled", sub.Status)
	}

	// The mock recorded customer create, subscription create,
	// metadata update, and cancel.
	want := map[string]bool{
		"POST /v1/customers":                false,
		"POST /v1/subscriptions":            false,
		"POST /v1/subscriptions/sub_test":   false,
		"DELETE /v1/subscriptions/sub_test": false,
	}
	for _, c := range *calls {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for c, seen := range want {
		if !seen {
			t.Errorf("expected Stripe call %q not observed; calls=%v", c, *calls)
		}
	}
}

func TestWithStripeNilReceiver(t *testing.T) {
	var lc *Lifecycle
	if got := lc.WithStripe(nil, nil); got != nil {
		t.Errorf("nil-receiver WithStripe should return nil")
	}
}

// TestOnPlanChangedSeedsLegacyTenantDB covers the branch where a
// tenant predates billing_subscriptions: OnPlanChanged seeds a fresh
// active row rather than failing.
func TestOnPlanChangedSeedsLegacyTenantDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()
	lc := NewLifecycle(svc, nil)

	// No OnTenantCreated → no subscription row yet.
	if _, err := lc.GetSubscription(ctx, tenant); err != ErrNotFound {
		t.Fatalf("precondition: want ErrNotFound, got %v", err)
	}
	if err := lc.OnPlanChanged(ctx, tenant, "", PlanPro); err != nil {
		t.Fatalf("OnPlanChanged seed: %v", err)
	}
	sub, err := lc.GetSubscription(ctx, tenant)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if sub.Plan != PlanPro || sub.Status != SubscriptionActive {
		t.Errorf("seeded sub plan=%q status=%s", sub.Plan, sub.Status)
	}
}
