package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLifecycleCreatePlanChangeDeleteDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()
	lc := NewLifecycle(svc, nil)

	// Deterministic clock so proration math is stable.
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	lc.now = func() time.Time { return base }

	if err := lc.OnTenantCreated(ctx, tenant, PlanCore); err != nil {
		t.Fatalf("OnTenantCreated: %v", err)
	}
	// Quota row + subscription row created.
	if _, err := svc.GetQuota(ctx, tenant); err != nil {
		t.Fatalf("quota after create: %v", err)
	}
	sub, err := lc.GetSubscription(ctx, tenant)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if sub.Plan != PlanCore || sub.Status != SubscriptionActive {
		t.Errorf("sub=%+v", sub)
	}

	// Invalid plan rejected.
	if err := lc.OnTenantCreated(ctx, tenant, "nope"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("OnTenantCreated bad plan=%v want ErrInvalidInput", err)
	}

	// Seat add/remove flow.
	if err := lc.OnSeatAdded(ctx, tenant); err != nil {
		t.Fatalf("OnSeatAdded: %v", err)
	}
	if err := lc.OnSeatRemoved(ctx, tenant); err != nil {
		t.Fatalf("OnSeatRemoved: %v", err)
	}

	// Proration preview for an upgrade (mid-period) should be positive.
	// Advance clock to mid-period.
	lc.now = func() time.Time { return base.AddDate(0, 0, 15) }
	preview, err := lc.ProrationPreview(ctx, tenant, PlanPro)
	if err != nil {
		t.Fatalf("ProrationPreview: %v", err)
	}
	if preview < 0 {
		t.Errorf("upgrade preview=%d want >= 0", preview)
	}

	// Change plan core → pro and record event.
	if _, err := svc.ChangePlan(ctx, tenant, PlanPro); err != nil {
		t.Fatalf("ChangePlan: %v", err)
	}
	if err := lc.OnPlanChanged(ctx, tenant, PlanCore, PlanPro); err != nil {
		t.Fatalf("OnPlanChanged: %v", err)
	}
	sub, _ = lc.GetSubscription(ctx, tenant)
	if sub.Plan != PlanPro {
		t.Errorf("sub plan after change=%s want pro", sub.Plan)
	}

	// History should include the plan_prorated event.
	hist, err := lc.ListBillingHistory(ctx, tenant, 0)
	if err != nil {
		t.Fatalf("ListBillingHistory: %v", err)
	}
	var sawProrated bool
	for _, e := range hist {
		if e.EventType == "plan_prorated" {
			sawProrated = true
		}
	}
	if !sawProrated {
		t.Errorf("history missing plan_prorated event: %+v", hist)
	}

	// Delete marks the subscription cancelled.
	if err := lc.OnTenantDeleted(ctx, tenant); err != nil {
		t.Fatalf("OnTenantDeleted: %v", err)
	}
	sub, _ = lc.GetSubscription(ctx, tenant)
	if sub.Status != SubscriptionCancelled {
		t.Errorf("sub status after delete=%s want cancelled", sub.Status)
	}
}

func TestOnPlanChangedSeedsSubscriptionDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()
	lc := NewLifecycle(svc, nil)

	// No subscription row yet (legacy tenant) → OnPlanChanged seeds one.
	if err := lc.OnPlanChanged(ctx, tenant, "", PlanPrivacy); err != nil {
		t.Fatalf("OnPlanChanged seed: %v", err)
	}
	sub, err := lc.GetSubscription(ctx, tenant)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if sub.Plan != PlanPrivacy || sub.Status != SubscriptionActive {
		t.Errorf("seeded sub=%+v", sub)
	}
}
