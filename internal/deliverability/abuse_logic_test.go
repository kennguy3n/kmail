package deliverability

import (
	"context"
	"testing"
)

func TestAbuseScorer_NilPoolPaths(t *testing.T) {
	ctx := context.Background()
	a := &AbuseScorer{} // nil pool

	// Validation guards fire before the nil-pool short circuit.
	if _, err := a.ScoreTenant(ctx, ""); err == nil {
		t.Error("ScoreTenant empty tenant should error")
	}
	if _, err := a.ScoreUser(ctx, "", "u"); err == nil {
		t.Error("ScoreUser empty tenant should error")
	}
	if _, err := a.DetectAnomalies(ctx, ""); err == nil {
		t.Error("DetectAnomalies empty tenant should error")
	}
	if _, err := a.ListAlerts(ctx, "", ListAlertsOptions{}); err == nil {
		t.Error("ListAlerts empty tenant should error")
	}
	if err := a.AcknowledgeAlert(ctx, "", "id"); err == nil {
		t.Error("AcknowledgeAlert empty tenant should error")
	}

	// Nil-pool stubs return zero-value successes.
	sc, err := a.ScoreTenant(ctx, "t1")
	if err != nil || sc.TenantID != "t1" {
		t.Fatalf("ScoreTenant nil-pool=%+v err=%v", sc, err)
	}
	// ScoreUser with empty userID delegates to ScoreTenant.
	if sc, err := a.ScoreUser(ctx, "t1", ""); err != nil || sc.UserID != "" {
		t.Fatalf("ScoreUser delegate=%+v err=%v", sc, err)
	}
	if sc, err := a.ScoreUser(ctx, "t1", "u1"); err != nil || sc.UserID != "u1" {
		t.Fatalf("ScoreUser nil-pool=%+v err=%v", sc, err)
	}
	if alerts, err := a.DetectAnomalies(ctx, "t1"); err != nil || alerts != nil {
		t.Fatalf("DetectAnomalies nil-pool=%v err=%v", alerts, err)
	}
	if alerts, err := a.ListAlerts(ctx, "t1", ListAlertsOptions{}); err != nil || alerts != nil {
		t.Fatalf("ListAlerts nil-pool=%v err=%v", alerts, err)
	}
	if err := a.AcknowledgeAlert(ctx, "t1", "id"); err != nil {
		t.Fatalf("AcknowledgeAlert nil-pool err=%v", err)
	}
}

func TestAbuseScorer_InsertAlertNilPool(t *testing.T) {
	a := &AbuseScorer{}
	alert, err := a.insertAlert(context.Background(), "t1", signalAlert{
		alertType: AlertTypeVolumeSpike, severity: SeverityHigh, score: 40,
		details: map[string]any{"k": "v"},
	})
	if err != nil || alert.ID != "stub" || alert.AlertType != AlertTypeVolumeSpike {
		t.Fatalf("insertAlert nil-pool=%+v err=%v", alert, err)
	}
}
