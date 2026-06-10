package calendarbridge

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fbServer returns a fake Stalwart that answers the REPORT used by
// FreeBusyService.Compute with a single busy VEVENT.
func fbServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, sampleEventsReportResponse)
	}))
}

func fbHandlers(t *testing.T) *FreeBusyHandlers {
	t.Helper()
	srv := fbServer(t)
	t.Cleanup(srv.Close)
	cal := NewService(Config{StalwartURL: srv.URL})
	return NewFreeBusyHandlers(NewFreeBusyService(cal), "https://kmail.example.com/", nil)
}

func fbReq(method, target string, body string, account, calendar string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if account != "" {
		r.SetPathValue("accountID", account)
	}
	if calendar != "" {
		r.SetPathValue("calendarID", calendar)
	}
	return r
}

func TestFreeBusyDiscovery(t *testing.T) {
	h := fbHandlers(t)
	rec := httptest.NewRecorder()
	h.discovery(rec, httptest.NewRequest(http.MethodGet, "/.well-known/caldav", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("discovery content-type=%q", ct)
	}
	if !strings.Contains(rec.Body.String(), "kmail.example.com") {
		t.Errorf("discovery body missing base url: %s", rec.Body.String())
	}
}

func TestFreeBusyJSON(t *testing.T) {
	h := fbHandlers(t)

	// Happy path with explicit window.
	rec := httptest.NewRecorder()
	r := fbReq(http.MethodGet, "/x?start=2026-05-01T00:00:00Z&end=2026-05-02T00:00:00Z", "", "alice", "default")
	h.json(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("json happy=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"busy"`) {
		t.Errorf("json body missing busy: %s", rec.Body.String())
	}

	// Defaulted window (no start/end) also succeeds.
	rec = httptest.NewRecorder()
	h.json(rec, fbReq(http.MethodGet, "/x", "", "alice", "default"))
	if rec.Code != http.StatusOK {
		t.Errorf("json default window=%d", rec.Code)
	}

	// Bad start ⇒ 400.
	rec = httptest.NewRecorder()
	h.json(rec, fbReq(http.MethodGet, "/x?start=nonsense", "", "alice", "default"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("json bad start=%d want 400", rec.Code)
	}

	// end <= start ⇒ 400.
	rec = httptest.NewRecorder()
	h.json(rec, fbReq(http.MethodGet, "/x?start=2026-05-02T00:00:00Z&end=2026-05-01T00:00:00Z", "", "alice", "default"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("json end<=start=%d want 400", rec.Code)
	}
}

func TestFreeBusyReport(t *testing.T) {
	h := fbHandlers(t)

	// Happy path.
	body := `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-freebusy xmlns:C="urn:ietf:params:xml:ns:caldav">
  <C:time-range start="20260501T000000Z" end="20260502T000000Z"/>
</C:calendar-freebusy>`
	rec := httptest.NewRecorder()
	h.report(rec, fbReq("REPORT", "/x", body, "alice", "default"))
	if rec.Code != http.StatusOK {
		t.Fatalf("report happy=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "VFREEBUSY") {
		t.Errorf("report body missing VFREEBUSY: %s", rec.Body.String())
	}

	// Malformed XML ⇒ 400.
	rec = httptest.NewRecorder()
	h.report(rec, fbReq("REPORT", "/x", "<not-xml", "alice", "default"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("report bad xml=%d want 400", rec.Code)
	}

	// Bad time-range format ⇒ 400.
	bad := `<C:calendar-freebusy xmlns:C="urn:ietf:params:xml:ns:caldav"><C:time-range start="nope" end="also-nope"/></C:calendar-freebusy>`
	rec = httptest.NewRecorder()
	h.report(rec, fbReq("REPORT", "/x", bad, "alice", "default"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("report bad time=%d want 400", rec.Code)
	}
}

func TestFreeBusyRegister(t *testing.T) {
	h := fbHandlers(t)
	mux := http.NewServeMux()
	h.Register(mux, passthroughAuth{})

	// Public discovery route is reachable without auth.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/caldav", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("routed discovery=%d", rec.Code)
	}

	// Auth-wrapped freebusy route is mounted.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/calendars/alice/default/freebusy?start=2026-05-01T00:00:00Z&end=2026-05-02T00:00:00Z", nil))
	if rec.Code == http.StatusNotFound {
		t.Errorf("freebusy route not mounted")
	}
}

// passthroughAuth lives in this package's tests already (audit uses a
// different package), so define a local minimal Registrar.
type passthroughAuth struct{}

func (passthroughAuth) Wrap(h http.Handler) http.Handler { return h }

var _ = log.Default
