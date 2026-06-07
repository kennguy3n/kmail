package calendarbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:evt-1\r\n" +
	"SUMMARY:Standup\r\nDTSTART:20240101T090000Z\r\nDTEND:20240101T093000Z\r\n" +
	"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:bob@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

// mockCalDAV returns an httptest server emulating the subset of
// Stalwart CalDAV the bridge speaks.
func mockCalDAV(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0"?>
<multistatus xmlns="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
  <response>
    <href>/dav/acct/calendars/work/</href>
    <propstat>
      <prop>
        <displayname>Work</displayname>
        <resourcetype><collection/><c:calendar/></resourcetype>
        <c:calendar-description>Work cal</c:calendar-description>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`))
		case "REPORT":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0"?>
<multistatus xmlns="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
  <response>
    <href>/dav/acct/calendars/work/evt-1.ics</href>
    <propstat>
      <prop>
        <c:calendar-data>` + sampleICS + `</c:calendar-data>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte(sampleICS))
		case http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServiceCalDAVRoundTrip(t *testing.T) {
	srv := mockCalDAV(t)
	svc := NewService(Config{StalwartURL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	ctx := context.Background()

	cals, err := svc.ListCalendars(ctx, "acct")
	if err != nil || len(cals) != 1 || cals[0].ID != "work" || cals[0].Name != "Work" {
		t.Fatalf("ListCalendars=%+v err=%v", cals, err)
	}

	events, err := svc.GetEvents(ctx, "acct", "work", TimeRange{})
	if err != nil || len(events) != 1 || events[0].UID != "evt-1" {
		t.Fatalf("GetEvents=%+v err=%v", events, err)
	}

	uid, err := svc.CreateEvent(ctx, "acct", "work", sampleICS)
	if err != nil || uid != "evt-1" {
		t.Fatalf("CreateEvent uid=%q err=%v", uid, err)
	}

	if err := svc.UpdateEvent(ctx, "acct", "work", "evt-1", sampleICS); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}

	if err := svc.DeleteEvent(ctx, "acct", "work", "evt-1"); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}

	// RespondToEvent fetches, rewrites PARTSTAT, then PUTs back.
	if err := svc.RespondToEvent(ctx, "acct", "work", "evt-1", "bob@example.com", ResponseAccepted); err != nil {
		t.Fatalf("RespondToEvent: %v", err)
	}
}

func TestServiceCalDAVValidation(t *testing.T) {
	svc := NewService(Config{StalwartURL: "http://unused"})
	ctx := context.Background()

	if _, err := svc.ListCalendars(ctx, ""); err == nil {
		t.Error("ListCalendars empty account must error")
	}
	if _, err := svc.GetEvents(ctx, "", "c", TimeRange{}); err == nil {
		t.Error("GetEvents empty account must error")
	}
	if _, err := svc.CreateEvent(ctx, "acct", "work", "no-uid-here"); err == nil {
		t.Error("CreateEvent without UID must error")
	}
	if err := svc.RespondToEvent(ctx, "acct", "work", "", "bob@example.com", ResponseAccepted); err == nil {
		t.Error("RespondToEvent empty uid must error")
	}
}

func TestServiceCalDAVNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	svc := NewService(Config{StalwartURL: srv.URL})
	ctx := context.Background()

	if _, err := svc.ListCalendars(ctx, "acct"); err != ErrNotFound {
		t.Errorf("ListCalendars 404 → %v want ErrNotFound", err)
	}
	if _, err := svc.GetEvents(ctx, "acct", "work", TimeRange{}); err != ErrNotFound {
		t.Errorf("GetEvents 404 → %v want ErrNotFound", err)
	}
	if err := svc.DeleteEvent(ctx, "acct", "work", "evt-1"); err != ErrNotFound {
		t.Errorf("DeleteEvent 404 → %v want ErrNotFound", err)
	}
}

func TestRewritePartstat(t *testing.T) {
	out, err := rewritePartstat(sampleICS, "bob@example.com", ResponseAccepted)
	if err != nil {
		t.Fatalf("rewritePartstat: %v", err)
	}
	if !strings.Contains(out, "PARTSTAT=ACCEPTED") {
		t.Errorf("expected PARTSTAT=ACCEPTED, got:\n%s", out)
	}
	if _, err := rewritePartstat(sampleICS, "nobody@example.com", ResponseAccepted); err == nil {
		t.Error("rewritePartstat with unknown attendee must error")
	}
	if _, err := rewritePartstat(sampleICS, "bob@example.com", "bogus"); err == nil {
		t.Error("rewritePartstat with bad response must error")
	}
}
