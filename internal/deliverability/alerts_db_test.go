package deliverability

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"
)

// TestAlertsEvaluateAndManageDB seeds breaching metrics, evaluates
// thresholds (which persists alerts), then exercises listing,
// filtering, acknowledgement, and threshold configuration.
func TestAlertsEvaluateAndManageDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	// 100 sends today + 10 hard bounces (10% ≥ 0.10 critical) +
	// 2 complaints (2% ≥ 0.003 critical).
	for i := 0; i < 100; i++ {
		if err := svc.SendLimit.CheckSendLimit(ctx, tenant); err != nil {
			t.Fatalf("CheckSendLimit #%d: %v", i, err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := svc.Bounce.ProcessBounce(ctx, tenant, BounceEvent{
			Email:      "hb" + string(rune('a'+i)) + "@example.com",
			BounceType: BounceHard,
		}); err != nil {
			t.Fatalf("ProcessBounce hard #%d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.Bounce.ProcessBounce(ctx, tenant, BounceEvent{
			Email:      "cp" + string(rune('a'+i)) + "@example.com",
			BounceType: BounceComplaint,
		}); err != nil {
			t.Fatalf("ProcessBounce complaint #%d: %v", i, err)
		}
	}

	raised, err := svc.Alerts.EvaluateThresholds(ctx, tenant)
	if err != nil {
		t.Fatalf("EvaluateThresholds: %v", err)
	}
	got := map[string]string{}
	for _, a := range raised {
		got[a.MetricName] = a.Severity
	}
	if got[MetricBounceRate] != AlertSeverityCritical {
		t.Errorf("bounce_rate severity=%q want critical (raised=%+v)", got[MetricBounceRate], raised)
	}
	if got[MetricComplaintRate] != AlertSeverityCritical {
		t.Errorf("complaint_rate severity=%q want critical", got[MetricComplaintRate])
	}

	all, err := svc.Alerts.ListAlerts(ctx, tenant, ListDeliverabilityAlertsOptions{})
	if err != nil || len(all) < 2 {
		t.Fatalf("ListAlerts all=%d err=%v", len(all), err)
	}

	// Filter: severity=critical and acknowledged=false.
	no := false
	crit, err := svc.Alerts.ListAlerts(ctx, tenant, ListDeliverabilityAlertsOptions{
		Severity: AlertSeverityCritical, Acknowledged: &no,
	})
	if err != nil || len(crit) == 0 {
		t.Fatalf("ListAlerts critical/unacked=%d err=%v", len(crit), err)
	}

	// Pagination.
	page, err := svc.Alerts.ListAlerts(ctx, tenant, ListDeliverabilityAlertsOptions{Limit: 1})
	if err != nil || len(page) != 1 {
		t.Fatalf("ListAlerts limit=1 n=%d err=%v", len(page), err)
	}

	// Acknowledge first, then it disappears from the unacked filter.
	if err := svc.Alerts.AcknowledgeAlert(ctx, tenant, all[0].ID); err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}
	after, err := svc.Alerts.ListAlerts(ctx, tenant, ListDeliverabilityAlertsOptions{Acknowledged: &no})
	if err != nil || len(after) != len(all)-1 {
		t.Fatalf("ListAlerts after-ack=%d want %d err=%v", len(after), len(all)-1, err)
	}

	// Ack non-existent ⇒ ErrNotFound.
	if err := svc.Alerts.AcknowledgeAlert(ctx, tenant, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AcknowledgeAlert missing=%v want ErrNotFound", err)
	}

	// ConfigureThresholds override is reflected by ListThresholds.
	if _, err := svc.Alerts.ConfigureThresholds(ctx, tenant, []AlertThreshold{
		{MetricName: MetricBounceRate, WarningThreshold: 0.01, CriticalThreshold: 0.02},
		{MetricName: ""}, // skipped
	}); err != nil {
		t.Fatalf("ConfigureThresholds: %v", err)
	}
	list, err := svc.Alerts.ListThresholds(ctx, tenant)
	if err != nil {
		t.Fatalf("ListThresholds: %v", err)
	}
	var found bool
	for _, th := range list {
		if th.MetricName == MetricBounceRate {
			found = th.WarningThreshold == 0.01 && th.CriticalThreshold == 0.02
		}
	}
	if !found {
		t.Errorf("bounce_rate override not reflected: %+v", list)
	}
}

// TestAlertEvaluatorRunDB exercises the background evaluator loop:
// an immediate first tick over all tenants, then prompt return on
// context cancellation.
func TestAlertEvaluatorRunDB(t *testing.T) {
	svc, _ := dbService(t)
	ev := &AlertEvaluator{
		Service:  svc.Alerts,
		Pool:     svc.cfg.Pool,
		Interval: 5 * time.Millisecond,
		Logger:   log.New(io.Discard, "", 0),
	}
	// tick directly: iterates active tenants without error.
	if err := ev.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ev.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}

	// Guard: Run returns immediately when misconfigured.
	(&AlertEvaluator{}).Run(context.Background())
}

func TestAlertService_NilPoolPaths(t *testing.T) {
	ctx := context.Background()
	a := &AlertService{} // nil pool

	if _, err := a.EvaluateThresholds(ctx, ""); err == nil {
		t.Error("EvaluateThresholds empty tenant should error")
	}
	if _, err := a.ListAlerts(ctx, "", ListDeliverabilityAlertsOptions{}); err == nil {
		t.Error("ListAlerts empty tenant should error")
	}
	if err := a.AcknowledgeAlert(ctx, "", "id"); err == nil {
		t.Error("AcknowledgeAlert empty should error")
	}
	if _, err := a.ListThresholds(ctx, ""); err == nil {
		t.Error("ListThresholds empty tenant should error")
	}

	// Nil-pool stubs: thresholds fall back to defaults, no DB writes.
	th, err := a.ListThresholds(ctx, "t1")
	if err != nil || len(th) != 4 {
		t.Fatalf("ListThresholds nil-pool=%d err=%v", len(th), err)
	}
	// EvaluateThresholds with nil pool samples zeroed metrics ⇒ no
	// alerts raised, no error.
	if raised, err := a.EvaluateThresholds(ctx, "t1"); err != nil || len(raised) != 0 {
		t.Fatalf("EvaluateThresholds nil-pool raised=%d err=%v", len(raised), err)
	}
	if alerts, err := a.ListAlerts(ctx, "t1", ListDeliverabilityAlertsOptions{}); err != nil || alerts != nil {
		t.Fatalf("ListAlerts nil-pool=%v err=%v", alerts, err)
	}
	if err := a.AcknowledgeAlert(ctx, "t1", "id"); err != nil {
		t.Fatalf("AcknowledgeAlert nil-pool err=%v", err)
	}
	if out, err := a.ConfigureThresholds(ctx, "t1", []AlertThreshold{{MetricName: MetricBounceRate}}); err != nil || len(out) != 1 {
		t.Fatalf("ConfigureThresholds nil-pool=%v err=%v", out, err)
	}
}
