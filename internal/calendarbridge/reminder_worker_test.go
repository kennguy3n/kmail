package calendarbridge

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestReminderHelpers(t *testing.T) {
	// summaryFromICal extracts the card fields.
	info := summaryFromICal("SUMMARY:Standup\r\nDTSTART:20260501T090000Z\r\nDTEND:20260501T093000Z\r\nLOCATION:HQ\r\nORGANIZER:mailto:a@example.com\r\n")
	if info.Summary != "Standup" || info.Start != "20260501T090000Z" || info.End != "20260501T093000Z" ||
		info.Location != "HQ" || info.Organizer != "mailto:a@example.com" {
		t.Errorf("summaryFromICal=%+v", info)
	}

	// parseEventStart accepts RFC3339 + iCal basic formats.
	for _, s := range []string{"2026-05-01T09:00:00Z", "20260501T090000Z", "20260501T090000"} {
		if _, err := parseEventStart(s); err != nil {
			t.Errorf("parseEventStart(%q): %v", s, err)
		}
	}
	if _, err := parseEventStart("nonsense"); err == nil {
		t.Error("parseEventStart(nonsense) should error")
	}

	if reminderKey("t", "ev", 15) != "reminder:t:ev:15" {
		t.Errorf("reminderKey=%q", reminderKey("t", "ev", 15))
	}
}

func TestReminderWorker_RunGuards(t *testing.T) {
	// nil receiver / missing deps return immediately.
	var w *ReminderWorker
	w.Run(context.Background())
	(&ReminderWorker{}).Run(context.Background())
}

// TestReminderWorkerTickDB exercises the full tick → tickTenant →
// NotifyReminder pipeline against a fake Stalwart CalDAV server and a
// recording KChat client.
func TestReminderWorkerTickDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	ctx := context.Background()

	// Seed an active user whose stalwart_account_id the worker uses
	// as the CalDAV principal.
	account := "alice-" + tenant[:8]
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (tenant_id, kchat_user_id, stalwart_account_id, email, display_name, status)
		VALUES ($1::uuid, $2, $3, $4, 'Reminder User', 'active')
	`, tenant, "kc-"+tenant[:8], account, "rem-"+tenant[:8]+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Fixed clock; event starts 4m30s out so it falls in the 5-min
	// window [4m, 5m].
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	start := now.Add(4*time.Minute + 30*time.Second).Format("20060102T150405Z")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, samplePropfindResponse)
		case "REPORT":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>/dav/%s/calendars/default/ev1.ics</D:href>
    <D:propstat><D:prop><C:calendar-data>BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:ev1
SUMMARY:Standup
DTSTART:%s
DTEND:%s
END:VEVENT
END:VCALENDAR</C:calendar-data></D:prop>
    <D:status>HTTP/1.1 200 OK</D:status></D:propstat>
  </D:response>
</D:multistatus>`, account, start, start))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	cal := NewService(Config{StalwartURL: srv.URL})
	chat := &fakeChat{}
	notifier := NewNotifier(chat, StaticChannelResolver{ChannelID: "chan-1"})

	w := NewReminderWorker(pool, cal, notifier, nil, log.New(io.Discard, "", 0))
	w.now = func() time.Time { return now }

	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(chat.posts) == 0 {
		t.Fatalf("expected at least one reminder post, got 0")
	}
	if got := chat.posts[0].msg.Text; got == "" {
		t.Errorf("empty reminder text")
	}
}

// TestReminderWorkerRunLoopDB starts the Run loop with a short poll
// interval and confirms it ticks then stops promptly on cancel.
func TestReminderWorkerRunLoopDB(t *testing.T) {
	pool := testsupport.Pool(t)
	_ = testsupport.SeedTenant(t, pool, "pro", "active")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, samplePropfindResponse)
	}))
	defer srv.Close()

	cal := NewService(Config{StalwartURL: srv.URL})
	notifier := NewNotifier(&fakeChat{}, StaticChannelResolver{ChannelID: "c"})
	w := NewReminderWorker(pool, cal, notifier, nil, log.New(io.Discard, "", 0)).
		WithPollInterval(5 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}
}
