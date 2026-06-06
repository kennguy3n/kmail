package calendarbridge

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

func calReq(account, method, body string, pv map[string]string) *http.Request {
	ctx := middleware.WithStalwartAccountID(context.Background(), account)
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(ctx, method, "/x", nil)
	} else {
		r = httptest.NewRequestWithContext(ctx, method, "/x", strings.NewReader(body))
	}
	for k, v := range pv {
		r.SetPathValue(k, v)
	}
	return r
}

func TestCalendarHandlersCalDAV(t *testing.T) {
	srv := mockCalDAV(t)
	svc := NewService(Config{StalwartURL: srv.URL})
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	pv := map[string]string{"accountID": "acct", "calendarID": "work", "eventUID": "evt-1"}

	// listCalendars
	rec := httptest.NewRecorder()
	h.listCalendars(rec, calReq("acct", http.MethodGet, "", pv))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "work") {
		t.Fatalf("listCalendars=%d body=%s", rec.Code, rec.Body.String())
	}

	// listEvents (with time range query)
	rec = httptest.NewRecorder()
	r := calReq("acct", http.MethodGet, "", pv)
	r.URL.RawQuery = "start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z"
	h.listEvents(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "evt-1") {
		t.Fatalf("listEvents=%d body=%s", rec.Code, rec.Body.String())
	}

	// createEvent
	rec = httptest.NewRecorder()
	h.createEvent(rec, calReq("acct", http.MethodPost, `{"icalData":`+jsonQuote(sampleICS)+`}`, pv))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "evt-1") {
		t.Fatalf("createEvent=%d body=%s", rec.Code, rec.Body.String())
	}

	// updateEvent
	rec = httptest.NewRecorder()
	h.updateEvent(rec, calReq("acct", http.MethodPut, `{"icalData":`+jsonQuote(sampleICS)+`}`, pv))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("updateEvent=%d body=%s", rec.Code, rec.Body.String())
	}

	// respondEvent
	rec = httptest.NewRecorder()
	h.respondEvent(rec, calReq("acct", http.MethodPost, `{"participant":"bob@example.com","response":"accepted"}`, pv))
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("respondEvent=%d body=%s", rec.Code, rec.Body.String())
	}

	// deleteEvent
	rec = httptest.NewRecorder()
	h.deleteEvent(rec, calReq("acct", http.MethodDelete, "", pv))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deleteEvent=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCalendarHandlersErrors(t *testing.T) {
	svc := NewService(Config{StalwartURL: "http://unused"})
	h := NewHandlers(svc, log.New(io.Discard, "", 0))

	// listCalendars without accountID → 400
	rec := httptest.NewRecorder()
	h.listCalendars(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("listCalendars no account=%d want 400", rec.Code)
	}

	// createEvent malformed JSON → 400
	rec = httptest.NewRecorder()
	h.createEvent(rec, calReq("acct", http.MethodPost, `{bad`, map[string]string{"accountID": "acct", "calendarID": "work"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createEvent bad json=%d want 400", rec.Code)
	}

	// createEvent with iCal lacking a UID → 400 (ErrInvalidInput)
	rec = httptest.NewRecorder()
	h.createEvent(rec, calReq("acct", http.MethodPost, `{"icalData":"no-uid"}`, map[string]string{"accountID": "acct", "calendarID": "work"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createEvent no uid=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
}

// jsonQuote returns a JSON string literal for s.
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
