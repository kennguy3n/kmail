package calendarbridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const samplePropfindResponse = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>/dav/alice/calendars/default/</D:href>
    <D:propstat>
      <D:prop>
        <D:displayname>Default</D:displayname>
        <D:resourcetype>
          <D:collection/>
          <C:calendar/>
        </D:resourcetype>
        <C:calendar-description>Default calendar</C:calendar-description>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`

const sampleEventsReportResponse = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:response>
    <D:href>/dav/alice/calendars/default/ev1.ics</D:href>
    <D:propstat>
      <D:prop>
        <C:calendar-data>BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:ev1
SUMMARY:Standup
DTSTART:20260501T090000Z
DTEND:20260501T093000Z
ATTENDEE;PARTSTAT=NEEDS-ACTION;CN=Alice:mailto:alice@example.com
END:VEVENT
END:VCALENDAR</C:calendar-data>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`

func newFakeStalwart(handler http.Handler) *httptest.Server {
	return httptest.NewServer(handler)
}

func TestListCalendars_ParsesMultistatus(t *testing.T) {
	srv := newFakeStalwart(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			t.Errorf("expected PROPFIND, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/dav/alice/calendars/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, samplePropfindResponse)
	}))
	defer srv.Close()

	svc := NewService(Config{StalwartURL: srv.URL})
	cals, err := svc.ListCalendars(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) != 1 {
		t.Fatalf("expected 1 calendar, got %d", len(cals))
	}
	if cals[0].ID != "default" {
		t.Errorf("ID = %q, want %q", cals[0].ID, "default")
	}
	if cals[0].Name != "Default" {
		t.Errorf("Name = %q, want %q", cals[0].Name, "Default")
	}
}

func TestGetEvents_ParsesReport(t *testing.T) {
	srv := newFakeStalwart(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "REPORT" {
			t.Errorf("expected REPORT, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "calendar-query") {
			t.Errorf("expected calendar-query body, got %s", body)
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, sampleEventsReportResponse)
	}))
	defer srv.Close()

	svc := NewService(Config{StalwartURL: srv.URL})
	evs, err := svc.GetEvents(context.Background(), "alice", "default", TimeRange{})
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0].UID != "ev1" || evs[0].Summary != "Standup" {
		t.Errorf("unexpected event: %+v", evs[0])
	}
}

const newVEVENT = `BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:new-event-uid
SUMMARY:Test
DTSTART:20260501T090000Z
DTEND:20260501T093000Z
END:VEVENT
END:VCALENDAR`

func TestCreateEvent_PutsICalData(t *testing.T) {
	var gotBody string
	srv := newFakeStalwart(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/new-event-uid.ics") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	svc := NewService(Config{StalwartURL: srv.URL})
	uid, err := svc.CreateEvent(context.Background(), "alice", "default", newVEVENT)
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if uid != "new-event-uid" {
		t.Errorf("UID = %q", uid)
	}
	if !strings.Contains(gotBody, "BEGIN:VEVENT") {
		t.Errorf("expected body to contain VEVENT; got %q", gotBody)
	}
}

func TestDeleteEvent_SendsDelete(t *testing.T) {
	srv := newFakeStalwart(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	svc := NewService(Config{StalwartURL: srv.URL})
	if err := svc.DeleteEvent(context.Background(), "alice", "default", "ev1"); err != nil {
		t.Errorf("DeleteEvent: %v", err)
	}
}

func TestRespondToEvent_RewritesPartstat(t *testing.T) {
	var putBody string
	srv := newFakeStalwart(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:ev1
ATTENDEE;PARTSTAT=NEEDS-ACTION;CN=Alice:mailto:alice@example.com
END:VEVENT
END:VCALENDAR`)
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			putBody = string(b)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()

	svc := NewService(Config{StalwartURL: srv.URL})
	if err := svc.RespondToEvent(context.Background(), "alice", "default", "ev1", "alice@example.com", ResponseAccepted); err != nil {
		t.Fatalf("RespondToEvent: %v", err)
	}
	if !strings.Contains(putBody, "PARTSTAT=ACCEPTED") {
		t.Errorf("expected PARTSTAT=ACCEPTED in PUT body, got %q", putBody)
	}
}

func TestExtractICalUID_MissingUID(t *testing.T) {
	_, err := extractICalUID("BEGIN:VCALENDAR\nBEGIN:VEVENT\nSUMMARY:foo\nEND:VEVENT\nEND:VCALENDAR")
	if err == nil {
		t.Error("expected missing-UID error")
	}
}

func TestExtractICalField_HandlesCRLFAndParams(t *testing.T) {
	ical := "BEGIN:VEVENT\r\nDTSTART;TZID=UTC:20260501T090000Z\r\nEND:VEVENT\r\n"
	got := extractICalField(ical, "DTSTART")
	if got != "20260501T090000Z" {
		t.Errorf("got %q, want 20260501T090000Z", got)
	}
}

func TestRewritePartstat_UnknownResponse(t *testing.T) {
	_, err := rewritePartstat("BEGIN:VEVENT\nATTENDEE:mailto:alice@example.com\nEND:VEVENT", "alice@example.com", "unknown")
	if err == nil {
		t.Error("expected ErrInvalidInput on unknown response")
	}
}

func TestCalendarIDFromHref(t *testing.T) {
	cases := map[string]string{
		"/dav/alice/calendars/default/":          "default",
		"/dav/alice/calendars/work-cal":          "work-cal",
		"http://host/dav/bob/calendars/personal": "personal",
	}
	for in, want := range cases {
		if got := calendarIDFromHref(in); got != want {
			t.Errorf("calendarIDFromHref(%q) = %q, want %q", in, got, want)
		}
	}
}
