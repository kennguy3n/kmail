package smartfeatures

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestAggregate_CountsAndTops(t *testing.T) {
	now := at("2026-01-10T12:00:00Z")
	sent := []Message{
		{ID: "s1", ReceivedAt: at("2026-01-01T09:00:00Z"), To: []Address{{Email: "alice@example.com", Name: "Alice"}}},
		{ID: "s2", ReceivedAt: at("2026-01-01T10:00:00Z"), To: []Address{{Email: "alice@example.com"}}, Cc: []Address{{Email: "bob@example.com"}}},
	}
	received := []Message{
		{ID: "r1", ReceivedAt: at("2026-01-01T08:00:00Z"), From: []Address{{Email: "carol@example.com", Name: "Carol"}}},
		{ID: "r2", ReceivedAt: at("2026-01-02T08:30:00Z"), From: []Address{{Email: "carol@example.com"}}},
	}

	a := Aggregate(sent, received, time.UTC, now)
	if a.TotalSent != 2 || a.TotalReceived != 2 {
		t.Fatalf("totals wrong: %+v", a)
	}
	if len(a.TopRecipients) == 0 || a.TopRecipients[0].Email != "alice@example.com" || a.TopRecipients[0].Count != 2 {
		t.Fatalf("top recipients wrong: %#v", a.TopRecipients)
	}
	if a.TopRecipients[0].Name != "Alice" {
		t.Fatalf("expected name backfill, got %#v", a.TopRecipients[0])
	}
	if a.TopSenders[0].Email != "carol@example.com" || a.TopSenders[0].Count != 2 {
		t.Fatalf("top senders wrong: %#v", a.TopSenders)
	}
	// Two received on 2026-01-01 (08:00) and 2026-01-02 (08:30).
	if len(a.BusiestHours) != 24 || a.BusiestHours[8].Count != 2 {
		t.Fatalf("busiest hours wrong: %#v", a.BusiestHours)
	}
	// Daily sorted ascending.
	if a.Daily[0].Date != "2026-01-01" || a.Daily[0].Sent != 2 || a.Daily[0].Received != 1 {
		t.Fatalf("daily[0] wrong: %#v", a.Daily)
	}
}

func TestAggregate_Timezone(t *testing.T) {
	now := at("2026-01-10T12:00:00Z")
	// 2026-01-02T01:00:00Z is 2026-01-01 20:00 in New York.
	received := []Message{{ID: "r", ReceivedAt: at("2026-01-02T01:00:00Z"), From: []Address{{Email: "x@y.com"}}}}
	ny, _ := time.LoadLocation("America/New_York")
	a := Aggregate(nil, received, ny, now)
	if a.BusiestHours[20].Count != 1 {
		t.Fatalf("expected hour 20 local, got %#v", a.BusiestHours)
	}
	if a.Daily[0].Date != "2026-01-01" {
		t.Fatalf("expected local day 2026-01-01, got %s", a.Daily[0].Date)
	}
}

func TestAggregate_ResponseTime(t *testing.T) {
	now := at("2026-01-10T12:00:00Z")
	received := []Message{
		{ID: "r1", ThreadID: "T1", ReceivedAt: at("2026-01-01T09:00:00Z"), From: []Address{{Email: "a@b.com"}}},
	}
	sent := []Message{
		// reply 1h later in the same thread
		{ID: "s1", ThreadID: "T1", ReceivedAt: at("2026-01-01T10:00:00Z"), To: []Address{{Email: "a@b.com"}}},
		// a sent message that started a different thread (no inbound) → ignored
		{ID: "s2", ThreadID: "T2", ReceivedAt: at("2026-01-01T11:00:00Z"), To: []Address{{Email: "c@d.com"}}},
	}
	a := Aggregate(sent, received, time.UTC, now)
	if a.ResponseSampleSize != 1 {
		t.Fatalf("sample size = %d, want 1", a.ResponseSampleSize)
	}
	if a.AvgResponseSeconds != 3600 {
		t.Fatalf("avg response = %v, want 3600", a.AvgResponseSeconds)
	}
}

func TestAggregate_ResponseTimeIgnoresUserInitiated(t *testing.T) {
	now := at("2026-01-10T12:00:00Z")
	// Sent precedes the inbound (user started thread) → no sample.
	received := []Message{{ID: "r1", ThreadID: "T1", ReceivedAt: at("2026-01-01T10:00:00Z"), From: []Address{{Email: "a@b.com"}}}}
	sent := []Message{{ID: "s1", ThreadID: "T1", ReceivedAt: at("2026-01-01T09:00:00Z"), To: []Address{{Email: "a@b.com"}}}}
	a := Aggregate(sent, received, time.UTC, now)
	if a.ResponseSampleSize != 0 || a.AvgResponseSeconds != 0 {
		t.Fatalf("expected no response sample, got %+v", a)
	}
}

func TestAggregate_Empty(t *testing.T) {
	a := Aggregate(nil, nil, time.UTC, at("2026-01-10T12:00:00Z"))
	if a.TotalSent != 0 || a.TotalReceived != 0 || len(a.Daily) != 0 {
		t.Fatalf("expected empty report, got %+v", a)
	}
	if len(a.BusiestHours) != 24 {
		t.Fatalf("busiest hours should always be 24 buckets")
	}
}

// TestAggregate_TotalsExcludeZeroTimestamps pins that the KPI totals
// count only messages with a usable timestamp, so they always equal
// the sum of the daily breakdown even when a message has a zero
// ReceivedAt (e.g. an unparseable receivedAt from Stalwart).
func TestAggregate_TotalsExcludeZeroTimestamps(t *testing.T) {
	now := at("2026-01-10T12:00:00Z")
	sent := []Message{
		{ID: "s1", ReceivedAt: at("2026-01-01T09:00:00Z"), To: []Address{{Email: "a@b.com"}}},
		{ID: "s2"}, // zero ReceivedAt — must not be counted
	}
	received := []Message{
		{ID: "r1", ReceivedAt: at("2026-01-01T08:00:00Z"), From: []Address{{Email: "c@d.com"}}},
		{ID: "r2"}, // zero ReceivedAt — must not be counted
	}
	a := Aggregate(sent, received, time.UTC, now)
	if a.TotalSent != 1 || a.TotalReceived != 1 {
		t.Fatalf("totals should skip zero-time msgs, got sent=%d received=%d", a.TotalSent, a.TotalReceived)
	}
	var daySent, dayReceived int
	for _, d := range a.Daily {
		daySent += d.Sent
		dayReceived += d.Received
	}
	if daySent != a.TotalSent || dayReceived != a.TotalReceived {
		t.Fatalf("totals must equal daily sums: total(%d,%d) daily(%d,%d)",
			a.TotalSent, a.TotalReceived, daySent, dayReceived)
	}
}

// fakeAnalyticsSource serves canned windows.
type fakeAnalyticsSource struct {
	sent, received []Message
	err            error
}

func (f *fakeAnalyticsSource) Window(_ context.Context, _, _ string, _ time.Time, _ int) ([]Message, []Message, error) {
	return f.sent, f.received, f.err
}

func TestAnalyticsHandler(t *testing.T) {
	src := &fakeAnalyticsSource{
		sent:     []Message{{ID: "s1", ReceivedAt: at("2026-01-01T09:00:00Z"), To: []Address{{Email: "a@b.com"}}}},
		received: []Message{{ID: "r1", ReceivedAt: at("2026-01-01T08:00:00Z"), From: []Address{{Email: "c@d.com"}}}},
	}
	h := NewAnalyticsHandlers(src, nil)
	h.now = func() time.Time { return at("2026-01-10T12:00:00Z") }

	r := httptest.NewRequest(http.MethodGet, "/api/v1/email-analytics?days=30", nil)
	r = r.WithContext(middleware.WithKChatUserID(middleware.WithTenantID(r.Context(), "t1"), "u1"))
	w := httptest.NewRecorder()
	h.report(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	out := decode(t, w)
	if out["total_sent"] != float64(1) || out["total_received"] != float64(1) {
		t.Fatalf("totals wrong: %v", out)
	}
	if out["range_start"] == "" {
		t.Fatalf("expected range_start set")
	}
}

func TestAnalyticsHandler_MissingAuth(t *testing.T) {
	h := NewAnalyticsHandlers(&fakeAnalyticsSource{}, nil)
	w := httptest.NewRecorder()
	h.report(w, httptest.NewRequest(http.MethodGet, "/api/v1/email-analytics", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
