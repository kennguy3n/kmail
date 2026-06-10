package export

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/calendarbridge"
	"github.com/kennguy3n/kmail/internal/jmap"
)

// fakeCalendar implements CalendarClient for the export runner.
type fakeCalendar struct {
	cals      []calendarbridge.Calendar
	events    map[string][]calendarbridge.Event
	eventsErr map[string]error
	listErr   error
}

func (f *fakeCalendar) ListCalendars(_ context.Context, _ string) ([]calendarbridge.Calendar, error) {
	return f.cals, f.listErr
}

func (f *fakeCalendar) GetEvents(_ context.Context, _, calendarID string, _ calendarbridge.TimeRange) ([]calendarbridge.Event, error) {
	if f.eventsErr != nil {
		if err := f.eventsErr[calendarID]; err != nil {
			return nil, err
		}
	}
	return f.events[calendarID], nil
}

// TestRun_FoldsCalendarICS exercises writeCalendars + extractVEvent:
// a wired CalendarClient folds each calendar into calendar/<id>.ics
// containing the VEVENT block carved out of the event's ICalData.
func TestRun_FoldsCalendarICS(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC)
	q := &fakeQuerier{ids: []string{"acct:1"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{
		"acct:1": msg("acct:1", "a@x", "One", now, "Subject: One\r\n\r\nbody\r\n"),
	}}
	up := &fakeUploader{}
	cal := &fakeCalendar{
		cals: []calendarbridge.Calendar{{ID: "cal-work", Name: "Work"}, {ID: "cal-broken", Name: "Broken"}},
		events: map[string][]calendarbridge.Event{
			"cal-work": {{
				UID:      "ev-1",
				ICalData: "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:ev-1\r\nSUMMARY:Standup\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
			}, {
				UID:      "ev-noevent",
				ICalData: "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n", // no VEVENT → skipped
			}},
		},
		// cal-broken's GetEvents errors → logged + skipped, not fatal.
		eventsErr: map[string]error{"cal-broken": errors.New("caldav 500")},
	}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: up, Calendar: cal})

	if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeAll}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	files := untar(t, up.gotBody)
	ics, ok := files["calendar/cal-work.ics"]
	if !ok {
		t.Fatalf("calendar/cal-work.ics missing; files=%v", keys(files))
	}
	if !bytes.Contains(ics, []byte("BEGIN:VEVENT")) || !bytes.Contains(ics, []byte("SUMMARY:Standup")) {
		t.Errorf("ics missing VEVENT block: %q", ics)
	}
	if !bytes.HasPrefix(ics, []byte("BEGIN:VCALENDAR")) {
		t.Errorf("ics missing VCALENDAR wrapper: %q", ics)
	}
	// The broken calendar is skipped (its GetEvents errored) but the
	// run still succeeds and writes an empty wrapper for it.
	if _, ok := files["calendar/cal-broken.ics"]; ok {
		t.Error("cal-broken should have been skipped, not written")
	}
}

// TestRun_CalendarListErrorFails verifies a ListCalendars failure
// aborts the export (it is not a best-effort skip).
func TestRun_CalendarListErrorFails(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{ids: []string{"acct:1"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{
		"acct:1": msg("acct:1", "a@x", "One", time.Now(), "Subject: One\r\n\r\nb\r\n"),
	}}
	cal := &fakeCalendar{listErr: errors.New("caldav unreachable")}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: &fakeUploader{}, Calendar: cal})

	if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeAll}); err == nil {
		t.Fatal("expected Run to fail when ListCalendars errors")
	}
}

func TestExtractVEvent(t *testing.T) {
	t.Parallel()
	in := "PREAMBLE\r\nBEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\nTRAILER"
	got := extractVEvent(in)
	if got != "BEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT" {
		t.Errorf("extractVEvent=%q", got)
	}
	if extractVEvent("no markers here") != "" {
		t.Error("missing markers should yield empty string")
	}
	if extractVEvent("END:VEVENT before BEGIN:VEVENT") != "" {
		t.Error("inverted markers should yield empty string")
	}
}
