package deliverability

import (
	"encoding/xml"
	"testing"
	"time"
)

func TestWarmupCapForDay(t *testing.T) {
	cases := []struct {
		day      int
		planCap  int
		warmup   int
		expected int
	}{
		{1, 5000, 30, 50},
		{2, 5000, 30, 100},
		{4, 5000, 30, 100},   // plateaus at the previous anchor
		{5, 5000, 30, 500},
		{10, 5000, 30, 1000},
		{19, 5000, 30, 1000}, // still on day-10 plateau
		{20, 5000, 30, 2000},
		{30, 5000, 30, 5000}, // full cap at day warmupDays
		{31, 5000, 30, 5000}, // post-ramp
	}
	for _, tc := range cases {
		got := WarmupCapForDay(tc.day, tc.planCap, tc.warmup)
		if got != tc.expected {
			t.Errorf("day=%d plan=%d: got %d want %d", tc.day, tc.planCap, got, tc.expected)
		}
	}
}

func TestWarmupCapClampsToPlanCap(t *testing.T) {
	// Core tier (500/day) should not climb above 500 during the
	// ramp even though the day-20 anchor is 2000.
	if got := WarmupCapForDay(20, 500, 30); got != 500 {
		t.Errorf("core day-20 ramp: got %d want 500", got)
	}
	if got := WarmupCapForDay(5, 500, 30); got != 500 {
		t.Errorf("core day-5 ramp: got %d want 500", got)
	}
}

func TestWarmupRampMonotonic(t *testing.T) {
	ramp := WarmupRamp(5000, 30)
	for d := 2; d <= 30; d++ {
		if ramp[d] < ramp[d-1] {
			t.Errorf("ramp not monotonic: day %d=%d < day %d=%d",
				d, ramp[d], d-1, ramp[d-1])
		}
	}
}

func TestHourlyFromDaily(t *testing.T) {
	cases := []struct{ daily, expected int }{
		{500, 50},
		{2000, 200},
		{5000, 500},
		{0, 0},
		{5, 1}, // rounds up to at least 1
	}
	for _, tc := range cases {
		got := HourlyFromDaily(tc.daily)
		if got != tc.expected {
			t.Errorf("daily=%d: got %d want %d", tc.daily, got, tc.expected)
		}
	}
}

func TestPlanDailyLimit(t *testing.T) {
	svc := &SendLimitService{coreDaily: 500, proDaily: 2000, privacyDaily: 5000}
	cases := []struct {
		plan     string
		expected int
		wantErr  bool
	}{
		{"core", 500, false},
		{"pro", 2000, false},
		{"privacy", 5000, false},
		{"enterprise", 0, true},
	}
	for _, tc := range cases {
		got, err := svc.PlanDailyLimit(tc.plan)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", tc.plan, err, tc.wantErr)
		}
		if got != tc.expected {
			t.Errorf("%s: got %d want %d", tc.plan, got, tc.expected)
		}
	}
}

func TestShouldEscalateSoft(t *testing.T) {
	b := &BounceProcessor{softEscalateCount: 3, softEscalateWin: 72 * time.Hour}
	cases := []struct {
		recent   int
		expected bool
	}{
		{0, false},
		{1, false},
		{2, false},
		{3, true},
		{10, true},
	}
	for _, tc := range cases {
		got := b.ShouldEscalateSoft(tc.recent)
		if got != tc.expected {
			t.Errorf("recent=%d: got %v want %v", tc.recent, got, tc.expected)
		}
	}
}

func TestIsValidReason(t *testing.T) {
	valid := []string{ReasonHardBounce, ReasonComplaint, ReasonManual, ReasonUnsubscribe}
	for _, r := range valid {
		if !isValidReason(r) {
			t.Errorf("reason %q should be valid", r)
		}
	}
	if isValidReason("made-up") {
		t.Error("unknown reason should be rejected")
	}
}

func TestIsValidPoolType(t *testing.T) {
	valid := []string{
		PoolSystemTransactional, PoolMatureTrusted, PoolNewWarming,
		PoolRestricted, PoolDedicatedEnterprise,
	}
	for _, pt := range valid {
		if !isValidPoolType(pt) {
			t.Errorf("pool type %q should be valid", pt)
		}
	}
	if isValidPoolType("bogus-pool") {
		t.Error("unknown pool type should be rejected")
	}
}

func TestSelectBestIP(t *testing.T) {
	ips := []IPAddress{
		{ID: "a", Address: "10.0.0.1", ReputationScore: 50, DailyVolume: 100, Status: "active"},
		{ID: "b", Address: "10.0.0.2", ReputationScore: 90, DailyVolume: 200, Status: "active"},
		{ID: "c", Address: "10.0.0.3", ReputationScore: 90, DailyVolume: 100, Status: "active"},
		{ID: "d", Address: "10.0.0.4", ReputationScore: 95, DailyVolume: 100, Status: "warming"},
	}
	best := selectBestIP(ips)
	if best == nil {
		t.Fatal("selectBestIP returned nil")
	}
	if best.ID != "c" {
		t.Errorf("selectBestIP picked %s, want c (highest reputation, lowest volume among active)", best.ID)
	}
}

func TestSelectBestIPNoActive(t *testing.T) {
	ips := []IPAddress{
		{ID: "a", Status: "warming"},
		{ID: "b", Status: "cooldown"},
	}
	if got := selectBestIP(ips); got != nil {
		t.Errorf("expected nil when no active IPs, got %v", got)
	}
}

func TestDMARCIngestRoundtrip(t *testing.T) {
	// Minimal valid aggregate DMARC report.
	sample := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<feedback>
  <report_metadata>
    <org_name>google.com</org_name>
    <email>noreply-dmarc-support@google.com</email>
    <report_id>12345</report_id>
    <date_range>
      <begin>1700000000</begin>
      <end>1700086400</end>
    </date_range>
  </report_metadata>
  <policy_published>
    <domain>example.com</domain>
    <adkim>r</adkim>
    <aspf>r</aspf>
    <p>reject</p>
  </policy_published>
  <record>
    <row>
      <source_ip>203.0.113.1</source_ip>
      <count>100</count>
      <policy_evaluated>
        <disposition>none</disposition>
        <dkim>pass</dkim>
        <spf>pass</spf>
      </policy_evaluated>
    </row>
    <identifiers>
      <header_from>example.com</header_from>
    </identifiers>
    <auth_results>
      <dkim>
        <domain>example.com</domain>
        <result>pass</result>
      </dkim>
      <spf>
        <domain>example.com</domain>
        <result>pass</result>
      </spf>
    </auth_results>
  </record>
  <record>
    <row>
      <source_ip>203.0.113.2</source_ip>
      <count>5</count>
      <policy_evaluated>
        <disposition>reject</disposition>
        <dkim>fail</dkim>
        <spf>fail</spf>
      </policy_evaluated>
    </row>
    <identifiers><header_from>example.com</header_from></identifiers>
    <auth_results/>
  </record>
</feedback>`)

	var agg aggregateReport
	if err := xml.Unmarshal(sample, &agg); err != nil {
		t.Fatalf("parse sample: %v", err)
	}
	if agg.PolicyPublished.Domain != "example.com" {
		t.Errorf("domain: got %q", agg.PolicyPublished.Domain)
	}
	if agg.ReportMetadata.OrgName != "google.com" {
		t.Errorf("org_name: got %q", agg.ReportMetadata.OrgName)
	}
	if len(agg.Records) != 2 {
		t.Fatalf("records: got %d want 2", len(agg.Records))
	}
	var pass, fail int64
	for _, r := range agg.Records {
		if r.Row.PolicyEvaluated.DKIM == "pass" || r.Row.PolicyEvaluated.SPF == "pass" {
			pass += r.Row.Count
		} else {
			fail += r.Row.Count
		}
	}
	if pass != 100 || fail != 5 {
		t.Errorf("pass/fail: got %d/%d want 100/5", pass, fail)
	}
}

func TestDMARCIngestInvalidXML(t *testing.T) {
	svc := &DMARCService{pool: nil}
	_, err := svc.IngestReport(nil, "tenant", []byte("not xml at all"))
	if err == nil {
		t.Fatal("expected error on invalid XML")
	}
}
