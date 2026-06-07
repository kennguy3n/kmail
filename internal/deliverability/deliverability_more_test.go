package deliverability

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ---------------------------------------------------------------
// Pure-logic tests (no infra)
// ---------------------------------------------------------------

func TestSeverityForRatio(t *testing.T) {
	cases := []struct {
		value, warn, crit float64
		want              string
	}{
		{0.1, 1, 2, SeverityLow},
		{1.0, 1, 2, SeverityMedium},
		{2.0, 1, 2, SeverityHigh},
		{3.0, 1, 2, SeverityCritical}, // >= crit*1.5
		{2.99, 1, 2, SeverityHigh},
	}
	for _, tc := range cases {
		if got := severityForRatio(tc.value, tc.warn, tc.crit); got != tc.want {
			t.Errorf("severityForRatio(%v,%v,%v)=%s want %s", tc.value, tc.warn, tc.crit, got, tc.want)
		}
	}
}

func TestEvaluateSeverityAndChooseThreshold(t *testing.T) {
	th := AlertThreshold{WarningThreshold: 0.05, CriticalThreshold: 0.10}
	if sev, crossed := evaluateSeverity(0.01, th); crossed || sev != "" {
		t.Errorf("below warning: got (%q,%v)", sev, crossed)
	}
	if sev, crossed := evaluateSeverity(0.06, th); !crossed || sev != AlertSeverityWarning {
		t.Errorf("warning: got (%q,%v)", sev, crossed)
	}
	if sev, crossed := evaluateSeverity(0.20, th); !crossed || sev != AlertSeverityCritical {
		t.Errorf("critical: got (%q,%v)", sev, crossed)
	}
	if got := chooseThreshold(AlertSeverityCritical, th); got != 0.10 {
		t.Errorf("chooseThreshold critical=%v want 0.10", got)
	}
	if got := chooseThreshold(AlertSeverityWarning, th); got != 0.05 {
		t.Errorf("chooseThreshold warning=%v want 0.05", got)
	}
}

func TestCompositeScore(t *testing.T) {
	// No signals → 0.
	if got := (abuseSignals{}).compositeScore(); got != 0 {
		t.Errorf("empty composite=%d want 0", got)
	}
	// Every signal tripped → clamped at 100 (20+15+25+20+20=100).
	full := abuseSignals{
		VolumeSpikeRatio:   5,
		NewDomainsRatio:    0.8,
		AuthFailuresLast5m: 50,
		BounceRate:         0.2,
		ComplaintRate:      0.01,
	}
	if got := full.compositeScore(); got != 100 {
		t.Errorf("full composite=%d want 100", got)
	}
	// Single signal.
	if got := (abuseSignals{BounceRate: 0.06}).compositeScore(); got != 20 {
		t.Errorf("bounce-only composite=%d want 20", got)
	}
}

func TestSignalsToAlerts(t *testing.T) {
	sig := abuseSignals{
		VolumeSpikeRatio:    4,
		NewDomainsRatio:     0.6,
		TotalDomainsLast24h: 10,
		AuthFailuresLast5m:  20,
		BounceRate:          0.08,
		ComplaintRate:       0.002,
	}
	alerts := sig.toAlerts("t", time.Now())
	got := map[string]bool{}
	for _, a := range alerts {
		got[a.alertType] = true
	}
	for _, want := range []string{
		AlertTypeVolumeSpike, AlertTypeRecipientAnomaly,
		AlertTypeAuthFailureStorm, AlertTypeHighBounceRate, AlertTypeHighComplaintRate,
	} {
		if !got[want] {
			t.Errorf("missing alert %s in %v", want, got)
		}
	}
	// New-domain anomaly requires TotalDomainsLast24h > 0.
	none := abuseSignals{NewDomainsRatio: 0.9, TotalDomainsLast24h: 0}.toAlerts("t", time.Now())
	for _, a := range none {
		if a.alertType == AlertTypeRecipientAnomaly {
			t.Error("recipient anomaly raised with zero total domains")
		}
	}
}

func TestParseARF(t *testing.T) {
	body := []byte("Feedback-Type: abuse\r\n" +
		"Source-IP: 192.0.2.1\r\n" +
		"Original-Rcpt-To: user@example.com\r\n" +
		"User-Agent: SomeFBL/1.0\r\n" +
		"\r\n" +
		"garbage-without-colon\r\n")
	r, err := ParseARF(body)
	if err != nil {
		t.Fatalf("ParseARF: %v", err)
	}
	if r.FeedbackType != "abuse" || r.SourceIP != "192.0.2.1" ||
		r.OriginalRcptTo != "user@example.com" || r.UserAgent != "SomeFBL/1.0" {
		t.Errorf("ParseARF mis-parsed: %+v", r)
	}
}

func TestDeriveDomainFromEmail(t *testing.T) {
	cases := map[string]string{
		"User@Example.COM": "example.com",
		"a@b@example.org":  "example.org",
		"no-at-sign":       "",
		"":                 "",
	}
	for in, want := range cases {
		if got := deriveDomainFromEmail(in); got != want {
			t.Errorf("deriveDomainFromEmail(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSplitHeader(t *testing.T) {
	n, v := splitHeader("Feedback-Type:  abuse  ")
	if n != "feedback-type" || v != "abuse" {
		t.Errorf("splitHeader got (%q,%q)", n, v)
	}
	if n, v := splitHeader("no-colon"); n != "" || v != "" {
		t.Errorf("splitHeader no-colon got (%q,%q)", n, v)
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got := normalizeEmail("  USER@Example.COM "); got != "user@example.com" {
		t.Errorf("normalizeEmail=%q", got)
	}
}

func TestThresholdsToSliceOrder(t *testing.T) {
	out := thresholdsToSlice(defaultThresholds)
	if len(out) != 4 {
		t.Fatalf("want 4 thresholds, got %d", len(out))
	}
	if out[0].MetricName != MetricBounceRate || out[3].MetricName != MetricDailyVolumeSpike {
		t.Errorf("unexpected threshold order: %+v", out)
	}
}

// ---------------------------------------------------------------
// Validation tests (no infra — nil pool / nil valkey paths)
// ---------------------------------------------------------------

func TestServiceDefaultsApplied(t *testing.T) {
	s := NewService(Config{})
	if s.cfg.WarmupDays != 30 || s.cfg.CoreDailyLimit != 500 ||
		s.cfg.ProDailyLimit != 2000 || s.cfg.PrivacyDailyLimit != 5000 {
		t.Errorf("defaults not applied: %+v", s.cfg)
	}
	if s.cfg.BounceSoftEscalationCount != 3 || s.cfg.BounceSoftWindow != 72*time.Hour {
		t.Errorf("bounce defaults not applied: %+v", s.cfg)
	}
}

func TestValidationErrors(t *testing.T) {
	s := NewService(Config{})
	ctx := context.Background()

	if _, err := s.Suppression.AddSuppression(ctx, "", "a@b.com", ReasonManual, "x"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("AddSuppression empty tenant: %v", err)
	}
	if _, err := s.Suppression.AddSuppression(ctx, "t", "a@b.com", "bogus", "x"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("AddSuppression bad reason: %v", err)
	}
	if _, err := s.Bounce.ProcessBounce(ctx, "t", BounceEvent{Email: "a@b.com", BounceType: "weird"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ProcessBounce bad type: %v", err)
	}
	if _, err := s.IPPool.CreatePool(ctx, CreatePoolInput{Name: "n", PoolType: "bad"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("CreatePool bad type: %v", err)
	}
	if err := s.SendLimit.SetLimit(ctx, "t", -1, 0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("SetLimit negative: %v", err)
	}
	if _, err := s.SendLimit.PlanDailyLimit("nope"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("PlanDailyLimit unknown: %v", err)
	}
	if _, err := s.DMARC.IngestReport(ctx, "t", []byte("not-xml")); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("IngestReport bad xml: %v", err)
	}
	if _, err := s.FeedbackLoop.ProcessGmailPostmasterData(ctx, "t", PostmasterData{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ProcessGmailPostmasterData empty domain: %v", err)
	}
}

// ---------------------------------------------------------------
// Valkey-backed tests (miniredis — always runs)
// ---------------------------------------------------------------

func newRedisService(t *testing.T, cfg Config) (*Service, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	cfg.Valkey = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewService(cfg), mr
}

func TestCheckSendLimitEnforcesHourlyCap(t *testing.T) {
	// Pro daily cap of 2 → hourly cap of 1 (HourlyFromDaily floors to 1).
	s, _ := newRedisService(t, Config{ProDailyLimit: 2})
	ctx := context.Background()
	const tenant = "11111111-1111-1111-1111-111111111111"

	if err := s.SendLimit.CheckSendLimit(ctx, tenant); err != nil {
		t.Fatalf("first send should pass: %v", err)
	}
	// Second send trips the hourly cap of 1.
	if err := s.SendLimit.CheckSendLimit(ctx, tenant); !errors.Is(err, ErrSendLimitExceeded) {
		t.Fatalf("second send should exceed: %v", err)
	}

	vol, err := s.SendLimit.GetDailyVolume(ctx, tenant, time.Now().UTC())
	if err != nil {
		t.Fatalf("GetDailyVolume: %v", err)
	}
	if vol != 2 {
		t.Errorf("daily volume=%d want 2", vol)
	}
}

func TestVolumeHistoryAndAverage(t *testing.T) {
	s, _ := newRedisService(t, Config{})
	ctx := context.Background()
	const tenant = "22222222-2222-2222-2222-222222222222"
	now := time.Now().UTC()

	// Seed yesterday=100, two-days-ago=200; today left unset (0).
	client := s.SendLimit.valkey
	client.Set(ctx, dailyKey(tenant, now.AddDate(0, 0, -1)), 100, 0)
	client.Set(ctx, dailyKey(tenant, now.AddDate(0, 0, -2)), 200, 0)

	hist, err := s.SendLimit.GetVolumeHistory(ctx, tenant, 3)
	if err != nil {
		t.Fatalf("GetVolumeHistory: %v", err)
	}
	if len(hist) != 3 || hist[0] != 0 || hist[1] != 100 || hist[2] != 200 {
		t.Fatalf("history=%v want [0 100 200]", hist)
	}

	// Average excludes today (index 0). Mean of [100,200] = 150.
	avg, err := s.SendLimit.AverageDailyVolume(ctx, tenant, 2)
	if err != nil {
		t.Fatalf("AverageDailyVolume: %v", err)
	}
	if avg != 150 {
		t.Errorf("avg=%v want 150", avg)
	}
}

func TestShouldEscalateSoftThreshold(t *testing.T) {
	s := NewService(Config{BounceSoftEscalationCount: 3})
	if s.Bounce.ShouldEscalateSoft(2) {
		t.Error("2 soft bounces should not escalate (threshold 3)")
	}
	if !s.Bounce.ShouldEscalateSoft(3) {
		t.Error("3 soft bounces should escalate")
	}
}

// ---------------------------------------------------------------
// Postgres-backed integration tests (skipped without a database)
// ---------------------------------------------------------------

func dbService(t *testing.T) (*Service, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	svc := NewService(Config{
		Pool:                      pool,
		Valkey:                    redis.NewClient(&redis.Options{Addr: mr.Addr()}),
		BounceSoftEscalationCount: 3,
		BounceSoftWindow:          72 * time.Hour,
	})
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	return svc, tenant
}

func TestSuppressionLifecycleDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	row, err := svc.Suppression.AddSuppression(ctx, tenant, "User@Example.com", ReasonManual, "test")
	if err != nil {
		t.Fatalf("AddSuppression: %v", err)
	}
	if row.Email != "user@example.com" {
		t.Errorf("email not normalized: %q", row.Email)
	}

	// Re-add with a different reason updates in place (no duplicate).
	if _, err := svc.Suppression.AddSuppression(ctx, tenant, "user@example.com", ReasonUnsubscribe, "test2"); err != nil {
		t.Fatalf("AddSuppression update: %v", err)
	}
	ok, err := svc.Suppression.IsSuppressed(ctx, tenant, "user@example.com")
	if err != nil || !ok {
		t.Fatalf("IsSuppressed=%v err=%v", ok, err)
	}
	list, err := svc.Suppression.ListSuppressions(ctx, tenant, ListSuppressionsOptions{})
	if err != nil {
		t.Fatalf("ListSuppressions: %v", err)
	}
	if len(list) != 1 || list[0].Reason != ReasonUnsubscribe {
		t.Fatalf("expected 1 row with updated reason, got %+v", list)
	}

	if err := svc.Suppression.RemoveSuppression(ctx, tenant, "user@example.com"); err != nil {
		t.Fatalf("RemoveSuppression: %v", err)
	}
	if err := svc.Suppression.RemoveSuppression(ctx, tenant, "user@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveSuppression second time: want ErrNotFound, got %v", err)
	}
	ok, _ = svc.Suppression.IsSuppressed(ctx, tenant, "user@example.com")
	if ok {
		t.Error("still suppressed after removal")
	}
}

func TestBounceHardAutoSuppressesDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	if _, err := svc.Bounce.ProcessBounce(ctx, tenant, BounceEvent{
		Email: "hard@example.com", BounceType: BounceHard, DSNCode: "5.1.1",
	}); err != nil {
		t.Fatalf("ProcessBounce hard: %v", err)
	}
	ok, err := svc.Suppression.IsSuppressed(ctx, tenant, "hard@example.com")
	if err != nil || !ok {
		t.Fatalf("hard bounce should auto-suppress: ok=%v err=%v", ok, err)
	}
}

func TestBounceSoftEscalationDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()
	email := "soft@example.com"

	// Below the escalation count (3) the recipient stays sendable.
	for i := 0; i < 2; i++ {
		if _, err := svc.Bounce.ProcessBounce(ctx, tenant, BounceEvent{Email: email, BounceType: BounceSoft}); err != nil {
			t.Fatalf("soft bounce %d: %v", i, err)
		}
	}
	if ok, _ := svc.Suppression.IsSuppressed(ctx, tenant, email); ok {
		t.Fatal("should not be suppressed after 2 soft bounces")
	}
	// Third soft bounce within the window escalates to suppression.
	if _, err := svc.Bounce.ProcessBounce(ctx, tenant, BounceEvent{Email: email, BounceType: BounceSoft}); err != nil {
		t.Fatalf("soft bounce 3: %v", err)
	}
	if ok, _ := svc.Suppression.IsSuppressed(ctx, tenant, email); !ok {
		t.Fatal("3rd soft bounce should escalate to suppression")
	}

	bounces, err := svc.Bounce.ListBounces(ctx, tenant, 10, 0)
	if err != nil {
		t.Fatalf("ListBounces: %v", err)
	}
	if len(bounces) != 3 {
		t.Errorf("want 3 bounce rows, got %d", len(bounces))
	}
}

func TestSendLimitOverrideDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	// Default: plan derives pro daily=2000.
	lim, err := svc.SendLimit.GetLimit(ctx, tenant)
	if err != nil {
		t.Fatalf("GetLimit: %v", err)
	}
	if lim.Plan != "pro" || lim.DailyLimit != 2000 {
		t.Errorf("default limit=%+v", lim)
	}
	// Override wins.
	if err := svc.SendLimit.SetLimit(ctx, tenant, 123, 45); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	lim, err = svc.SendLimit.GetLimit(ctx, tenant)
	if err != nil {
		t.Fatalf("GetLimit after override: %v", err)
	}
	if lim.DailyLimit != 123 || lim.HourlyLimit != 45 {
		t.Errorf("override limit=%+v want 123/45", lim)
	}
}

func TestIPPoolLifecycleDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	name := "warm-" + itoa(time.Now().UnixNano())
	pool, err := svc.IPPool.CreatePool(ctx, CreatePoolInput{Name: name, PoolType: PoolNewWarming, Description: "d"})
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	// ip_pools is a global (non-tenant) table, so clean up explicitly.
	t.Cleanup(func() {
		rawPool := testsupport.Pool(t)
		_, _ = rawPool.Exec(context.Background(), `DELETE FROM ip_pools WHERE id = $1::uuid`, pool.ID)
	})
	n := time.Now().UnixNano()
	addr := "10." + itoa((n>>16)&255) + "." + itoa((n>>8)&255) + "." + itoa(n&255)
	if _, err := svc.IPPool.AddIP(ctx, pool.ID, AddIPInput{Address: addr, ReverseDNS: "mx.example.com"}); err != nil {
		t.Fatalf("AddIP: %v", err)
	}
	ips, err := svc.IPPool.ListIPs(ctx, pool.ID)
	if err != nil || len(ips) != 1 {
		t.Fatalf("ListIPs=%v err=%v", ips, err)
	}

	if err := svc.IPPool.AssignTenantPool(ctx, tenant, PoolNewWarming, 10); err != nil {
		t.Fatalf("AssignTenantPool: %v", err)
	}
	assign, err := svc.IPPool.GetTenantPool(ctx, tenant)
	if err != nil {
		t.Fatalf("GetTenantPool: %v", err)
	}
	if assign.PoolType != PoolNewWarming || assign.Priority != 10 {
		t.Errorf("assignment=%+v", assign)
	}
	// The seeded IP defaults to status 'warming', so SelectSendingIP
	// finds no active IP and returns ErrNotFound.
	if _, err := svc.IPPool.SelectSendingIP(ctx, tenant); !errors.Is(err, ErrNotFound) {
		t.Errorf("SelectSendingIP with no active IP: want ErrNotFound, got %v", err)
	}
}

func TestDMARCIngestAndSummaryDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	// Use a recent window so the 30-day GetReportSummary includes it.
	begin := time.Now().Add(-24 * time.Hour).Unix()
	end := time.Now().Add(-23 * time.Hour).Unix()
	xml := []byte(`<?xml version="1.0"?>
<feedback>
  <report_metadata>
    <org_name>google.com</org_name>
    <email>noreply-dmarc@google.com</email>
    <report_id>rpt-1</report_id>
    <date_range><begin>` + itoa(begin) + `</begin><end>` + itoa(end) + `</end></date_range>
  </report_metadata>
  <policy_published><domain>example.com</domain><adkim>r</adkim><aspf>r</aspf><p>none</p></policy_published>
  <record>
    <row><source_ip>192.0.2.1</source_ip><count>10</count>
      <policy_evaluated><disposition>none</disposition><dkim>pass</dkim><spf>pass</spf></policy_evaluated></row>
    <identifiers><header_from>example.com</header_from></identifiers>
  </record>
  <record>
    <row><source_ip>198.51.100.9</source_ip><count>4</count>
      <policy_evaluated><disposition>quarantine</disposition><dkim>fail</dkim><spf>fail</spf></policy_evaluated></row>
    <identifiers><header_from>example.com</header_from></identifiers>
  </record>
</feedback>`)

	rep, err := svc.DMARC.IngestReport(ctx, tenant, xml)
	if err != nil {
		t.Fatalf("IngestReport: %v", err)
	}
	if rep.PassCount != 10 || rep.FailCount != 4 {
		t.Errorf("pass/fail=%d/%d want 10/4", rep.PassCount, rep.FailCount)
	}
	reports, err := svc.DMARC.ListReports(ctx, tenant, "", 50, 0)
	if err != nil || len(reports) != 1 {
		t.Fatalf("ListReports=%v err=%v", reports, err)
	}
	sum, err := svc.DMARC.GetReportSummary(ctx, tenant, "")
	if err != nil {
		t.Fatalf("GetReportSummary: %v", err)
	}
	if sum.PassCount != 10 || sum.FailCount != 4 || sum.ReportCount != 1 {
		t.Errorf("summary=%+v", sum)
	}
}

func TestFeedbackLoopDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	evt, err := svc.FeedbackLoop.ProcessGmailPostmasterData(ctx, tenant, PostmasterData{
		Domain: "example.com", SpamRate: 0.01, IPReputation: "HIGH",
	})
	if err != nil {
		t.Fatalf("ProcessGmailPostmasterData: %v", err)
	}
	if evt.EventType != "high_spam_rate" {
		t.Errorf("event type=%q want high_spam_rate (spam_rate above 0.003)", evt.EventType)
	}
	if _, err := svc.FeedbackLoop.ProcessYahooARF(ctx, tenant, ARFReport{
		OriginalRcptTo: "abuse@example.com",
	}); err != nil {
		t.Fatalf("ProcessYahooARF: %v", err)
	}
	events, err := svc.FeedbackLoop.ListFeedbackEvents(ctx, tenant, ListFeedbackEventsOptions{})
	if err != nil {
		t.Fatalf("ListFeedbackEvents: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("want 2 feedback events, got %d", len(events))
	}
}

func TestAbuseScoringDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	score, err := svc.Abuse.ScoreTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("ScoreTenant: %v", err)
	}
	if score.TenantID != tenant {
		t.Errorf("score tenant mismatch: %+v", score)
	}
	// Idempotent re-score (ON CONFLICT upsert).
	if _, err := svc.Abuse.ScoreTenant(ctx, tenant); err != nil {
		t.Fatalf("ScoreTenant re-run: %v", err)
	}
	if _, err := svc.Abuse.ListAlerts(ctx, tenant, ListAlertsOptions{}); err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
}

func TestAlertThresholdsDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	// Defaults merged when no override exists.
	defaults, err := svc.Alerts.ListThresholds(ctx, tenant)
	if err != nil || len(defaults) != 4 {
		t.Fatalf("ListThresholds defaults=%v err=%v", defaults, err)
	}
	out, err := svc.Alerts.ConfigureThresholds(ctx, tenant, []AlertThreshold{
		{MetricName: MetricBounceRate, WarningThreshold: 0.02, CriticalThreshold: 0.07},
		{MetricName: ""}, // skipped
	})
	if err != nil {
		t.Fatalf("ConfigureThresholds: %v", err)
	}
	var found bool
	for _, th := range out {
		if th.MetricName == MetricBounceRate {
			found = true
			if th.WarningThreshold != 0.02 || th.CriticalThreshold != 0.07 {
				t.Errorf("override not applied: %+v", th)
			}
		}
	}
	if !found {
		t.Error("bounce_rate threshold missing after configure")
	}
	if _, err := svc.Alerts.EvaluateThresholds(ctx, tenant); err != nil {
		t.Fatalf("EvaluateThresholds: %v", err)
	}
}

func TestWarmupGetCurrentLimitDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()
	// Tenant created just now → day 1 of the ramp → 50.
	cap, err := svc.Warmup.GetCurrentLimit(ctx, tenant)
	if err != nil {
		t.Fatalf("GetCurrentLimit: %v", err)
	}
	if cap != 50 {
		t.Errorf("day-1 warmup cap=%d want 50", cap)
	}
}
