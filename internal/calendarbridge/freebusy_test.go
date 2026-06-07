package calendarbridge

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const twoBusyEventsReport = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>/dav/alice/calendars/default/ev1.ics</D:href>
    <D:propstat><D:prop><C:calendar-data>BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:ev1
DTSTART:20260501T090000Z
DTEND:20260501T093000Z
END:VEVENT
END:VCALENDAR</C:calendar-data></D:prop></D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/alice/calendars/default/ev2.ics</D:href>
    <D:propstat><D:prop><C:calendar-data>BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:ev2
DTSTART:20260501T092000Z
DTEND:20260501T100000Z
END:VEVENT
END:VCALENDAR</C:calendar-data></D:prop></D:propstat>
  </D:response>
</D:multistatus>`

func TestFreeBusyComputeMergesAndRenders(t *testing.T) {
	srv := newFakeStalwart(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, twoBusyEventsReport)
	}))
	defer srv.Close()

	fb := NewFreeBusyService(NewService(Config{StalwartURL: srv.URL}))
	start := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	res, err := fb.Compute(context.Background(), "alice", "default", start, end)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	// ev1 [09:00,09:30) overlaps ev2 [09:20,10:00) → merged into one [09:00,10:00).
	if len(res.Busy) != 1 {
		t.Fatalf("expected 1 merged interval, got %d: %+v", len(res.Busy), res.Busy)
	}
	if !res.Busy[0].Start.Equal(time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)) ||
		!res.Busy[0].End.Equal(time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("merged interval wrong: %+v", res.Busy[0])
	}

	ical := res.AsICalendar()
	if !strings.Contains(ical, "BEGIN:VFREEBUSY") ||
		!strings.Contains(ical, "FREEBUSY;FBTYPE=BUSY:20260501T090000Z/20260501T100000Z") {
		t.Errorf("unexpected VFREEBUSY render:\n%s", ical)
	}
}

func TestFreeBusyComputeValidation(t *testing.T) {
	fb := NewFreeBusyService(NewService(Config{StalwartURL: "http://unused"}))
	now := time.Now()
	if _, err := fb.Compute(context.Background(), "a", "c", now, now); err == nil {
		t.Error("equal start/end: expected error")
	}
	if _, err := fb.Compute(context.Background(), "a", "c", now, now.Add(-time.Hour)); err == nil {
		t.Error("end before start: expected error")
	}
}
