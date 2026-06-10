package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type recordingNotifier struct {
	calls int
}

func (n *recordingNotifier) NotifyPaymentFailed(_ context.Context, _, _ string, _ int64, _ string) error {
	n.calls++
	return nil
}

type recordingAuditor struct {
	actions []string
}

func (a *recordingAuditor) Log(_ context.Context, _, _, action, _ string, _ map[string]any) error {
	a.actions = append(a.actions, action)
	return nil
}

func tenantStatus(t *testing.T, ctx context.Context, svc *Service, tenant string) string {
	t.Helper()
	var status string
	if err := svc.cfg.Pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1::uuid`, tenant).Scan(&status); err != nil {
		t.Fatalf("read tenant status: %v", err)
	}
	return status
}

func TestDunningSuspendsAfterThresholdDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()
	notifier := &recordingNotifier{}
	auditor := &recordingAuditor{}
	base := time.Now()
	ds := NewDunningService(DunningConfig{
		Pool:     svc.cfg.Pool,
		Notifier: notifier,
		Auditor:  auditor,
		Now:      func() time.Time { return base },
	})

	// Two distinct failures: notified twice, not yet suspended.
	for i := 0; i < 2; i++ {
		evt := DunningEvent{
			TenantID:        tenant,
			StripeInvoiceID: fmt.Sprintf("in_%d", i),
			AmountDue:       1500,
			Currency:        "usd",
			OccurredAt:      base,
		}
		if err := ds.Handle(ctx, evt); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	if notifier.calls != 2 {
		t.Errorf("notifier calls=%d want 2", notifier.calls)
	}
	if got := tenantStatus(t, ctx, svc, tenant); got != "active" {
		t.Errorf("status after 2 failures=%s want active", got)
	}

	// Third distinct failure crosses the threshold → suspended.
	if err := ds.Handle(ctx, DunningEvent{TenantID: tenant, StripeInvoiceID: "in_2", AmountDue: 1500, Currency: "usd", OccurredAt: base}); err != nil {
		t.Fatalf("Handle 3: %v", err)
	}
	if got := tenantStatus(t, ctx, svc, tenant); got != "suspended" {
		t.Errorf("status after 3 failures=%s want suspended", got)
	}

	// A retry of an already-recorded invoice id is deduped: no new
	// notification, no double-suspend audit entry.
	before := notifier.calls
	if err := ds.Handle(ctx, DunningEvent{TenantID: tenant, StripeInvoiceID: "in_2", AmountDue: 1500, Currency: "usd", OccurredAt: base}); err != nil {
		t.Fatalf("Handle retry: %v", err)
	}
	if notifier.calls != before {
		t.Errorf("retry triggered notification: calls=%d want %d", notifier.calls, before)
	}

	// Audit log saw the failures plus a suspension.
	var sawSuspend bool
	for _, a := range auditor.actions {
		if a == "billing.tenant_suspended" {
			sawSuspend = true
		}
	}
	if !sawSuspend {
		t.Errorf("audit actions missing suspension: %v", auditor.actions)
	}
}

func TestDunningGuards(t *testing.T) {
	ctx := context.Background()

	// Empty tenant id is rejected.
	ds := NewDunningService(DunningConfig{})
	if err := ds.Handle(ctx, DunningEvent{}); err == nil {
		t.Error("empty tenant should error")
	}

	// Nil pool: recordAndCount returns (1,true); a single failure does
	// not hit the threshold, so Handle just notifies once and returns.
	notifier := &recordingNotifier{}
	ds = NewDunningService(DunningConfig{Notifier: notifier})
	if err := ds.Handle(ctx, DunningEvent{TenantID: "t1", StripeInvoiceID: "in_x"}); err != nil {
		t.Fatalf("nil-pool Handle: %v", err)
	}
	if notifier.calls != 1 {
		t.Errorf("nil-pool notifier calls=%d want 1", notifier.calls)
	}

	// OccurredAt defaults to Now when zero — exercised by the call above
	// (no panic, no error). A notifier returning an error is logged, not
	// propagated.
	ds = NewDunningService(DunningConfig{Notifier: errNotifier{}})
	if err := ds.Handle(ctx, DunningEvent{TenantID: "t1", StripeInvoiceID: "in_y"}); err != nil {
		t.Errorf("notifier error should be swallowed, got %v", err)
	}
}

type errNotifier struct{}

func (errNotifier) NotifyPaymentFailed(_ context.Context, _, _ string, _ int64, _ string) error {
	return errors.New("kchat down")
}
