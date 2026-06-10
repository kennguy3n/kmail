package deliverability

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// TestAbuseDetectAndAlertsDB drives the full anomaly pipeline against
// a live DB + Valkey: seed enough send volume and hard/complaint
// bounces to breach the bounce- and complaint-rate thresholds, then
// assert DetectAnomalies persists alerts that ListAlerts can filter
// and AcknowledgeAlert can flip.
func TestAbuseDetectAndAlertsDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	// 100 sends today (CheckSendLimit increments the daily counter).
	for i := 0; i < 100; i++ {
		if err := svc.SendLimit.CheckSendLimit(ctx, tenant); err != nil {
			t.Fatalf("CheckSendLimit #%d: %v", i, err)
		}
	}
	// 10 hard bounces (10% bounce rate ≥ 0.05) + 2 complaints
	// (2% complaint rate ≥ 0.001).
	for i := 0; i < 10; i++ {
		if _, err := svc.Bounce.ProcessBounce(ctx, tenant, BounceEvent{
			Email:      "hard" + string(rune('a'+i)) + "@example.com",
			BounceType: BounceHard,
		}); err != nil {
			t.Fatalf("ProcessBounce hard #%d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.Bounce.ProcessBounce(ctx, tenant, BounceEvent{
			Email:      "comp" + string(rune('a'+i)) + "@example.com",
			BounceType: BounceComplaint,
		}); err != nil {
			t.Fatalf("ProcessBounce complaint #%d: %v", i, err)
		}
	}

	raised, err := svc.Abuse.DetectAnomalies(ctx, tenant)
	if err != nil {
		t.Fatalf("DetectAnomalies: %v", err)
	}
	types := map[string]bool{}
	for _, a := range raised {
		types[a.AlertType] = true
	}
	if !types[AlertTypeHighBounceRate] {
		t.Errorf("expected high_bounce_rate alert, got %v", types)
	}
	if !types[AlertTypeHighComplaintRate] {
		t.Errorf("expected high_complaint_rate alert, got %v", types)
	}

	// ListAlerts: unfiltered returns the raised rows.
	all, err := svc.Abuse.ListAlerts(ctx, tenant, ListAlertsOptions{})
	if err != nil || len(all) < 2 {
		t.Fatalf("ListAlerts all=%d err=%v", len(all), err)
	}

	// Filter by acknowledged=false ⇒ all are unacked initially.
	no := false
	unacked, err := svc.Abuse.ListAlerts(ctx, tenant, ListAlertsOptions{Acknowledged: &no})
	if err != nil || len(unacked) != len(all) {
		t.Fatalf("ListAlerts unacked=%d want %d err=%v", len(unacked), len(all), err)
	}

	// Filter by the severity of the first row ⇒ at least that row.
	bySeverity, err := svc.Abuse.ListAlerts(ctx, tenant, ListAlertsOptions{Severity: all[0].Severity})
	if err != nil || len(bySeverity) == 0 {
		t.Fatalf("ListAlerts severity=%q n=%d err=%v", all[0].Severity, len(bySeverity), err)
	}

	// Pagination: limit 1 returns exactly one row.
	page, err := svc.Abuse.ListAlerts(ctx, tenant, ListAlertsOptions{Limit: 1})
	if err != nil || len(page) != 1 {
		t.Fatalf("ListAlerts limit=1 n=%d err=%v", len(page), err)
	}

	// Acknowledge the first alert, then it drops out of the
	// acknowledged=false filter.
	if err := svc.Abuse.AcknowledgeAlert(ctx, tenant, all[0].ID); err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}
	after, err := svc.Abuse.ListAlerts(ctx, tenant, ListAlertsOptions{Acknowledged: &no})
	if err != nil || len(after) != len(all)-1 {
		t.Fatalf("ListAlerts after-ack=%d want %d err=%v", len(after), len(all)-1, err)
	}

	// Acknowledging a non-existent alert ⇒ ErrNotFound.
	if err := svc.Abuse.AcknowledgeAlert(ctx, tenant, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AcknowledgeAlert missing=%v want ErrNotFound", err)
	}

	// ScoreUser persists a per-user row distinct from the tenant row.
	userID := seedUser(t, svc.cfg.Pool, tenant)
	us, err := svc.Abuse.ScoreUser(ctx, tenant, userID)
	if err != nil {
		t.Fatalf("ScoreUser: %v", err)
	}
	if us.UserID != userID {
		t.Errorf("ScoreUser user mismatch: %+v", us)
	}
}

// seedUser inserts a minimal active user for the tenant (under the
// tenant GUC so RLS permits the write) and returns its id.
func seedUser(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		suffix := tenantID[:8]
		return tx.QueryRow(ctx, `
			INSERT INTO users (tenant_id, kchat_user_id, stalwart_account_id, email, display_name)
			VALUES ($1::uuid, $2, $3, $4, 'Abuse Test User')
			RETURNING id::text
		`, tenantID, "kc-"+suffix, "sw-"+suffix, "abuse-"+suffix+"@example.com").Scan(&id)
	})
	if err != nil {
		t.Fatalf("seedUser: %v", err)
	}
	return id
}
