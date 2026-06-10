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

// calReqTenant is like calReq but also injects a tenant id so
// notifyEvent dispatches instead of short-circuiting.
func calReqTenant(tenant, account, method, body string, pv map[string]string) *http.Request {
	ctx := middleware.WithStalwartAccountID(context.Background(), account)
	ctx = middleware.WithTenantID(ctx, tenant)
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

// TestCalendarHandlersNotify drives create/update/delete with a wired
// Notifier so the notifyEvent created/updated/cancelled branches all
// post to KChat.
func TestCalendarHandlersNotify(t *testing.T) {
	srv := mockCalDAV(t)
	svc := NewService(Config{StalwartURL: srv.URL})
	chat := &fakeChat{}
	notifier := NewNotifier(chat, StaticChannelResolver{ChannelID: "chan-1"})
	h := NewHandlers(svc, log.New(io.Discard, "", 0)).WithNotifier(notifier)
	pv := map[string]string{"accountID": "acct", "calendarID": "work", "eventUID": "evt-1"}

	rec := httptest.NewRecorder()
	h.createEvent(rec, calReqTenant("11111111-1111-1111-1111-111111111111", "acct", http.MethodPost, `{"icalData":`+jsonQuote(sampleICS)+`}`, pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("createEvent=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.updateEvent(rec, calReqTenant("11111111-1111-1111-1111-111111111111", "acct", http.MethodPut, `{"icalData":`+jsonQuote(sampleICS)+`}`, pv))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("updateEvent=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.deleteEvent(rec, calReqTenant("11111111-1111-1111-1111-111111111111", "acct", http.MethodDelete, "", pv))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deleteEvent=%d body=%s", rec.Code, rec.Body.String())
	}

	if len(chat.posts) != 3 {
		t.Fatalf("expected 3 KChat posts (create/update/cancel), got %d", len(chat.posts))
	}

	// notifyEvent with no tenant in context is a no-op (no extra post).
	rec = httptest.NewRecorder()
	h.createEvent(rec, calReq("acct", http.MethodPost, `{"icalData":`+jsonQuote(sampleICS)+`}`, pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("createEvent no-tenant=%d", rec.Code)
	}
	if len(chat.posts) != 3 {
		t.Errorf("no-tenant create should not post; posts=%d", len(chat.posts))
	}
}

// TestCalendarHandlersRegister verifies routes mount + flow through
// the passthrough auth wrapper.
func TestCalendarHandlersRegister(t *testing.T) {
	srv := mockCalDAV(t)
	svc := NewService(Config{StalwartURL: srv.URL})
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	mux := http.NewServeMux()
	h.Register(mux, passthroughAuth{})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/calendars/acct", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("routed listCalendars=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCalendarHandlersUpstreamError maps a transport failure to 502.
func TestCalendarHandlersUpstreamError(t *testing.T) {
	// Point at a closed port so the CalDAV round-trip fails.
	svc := NewService(Config{StalwartURL: "http://127.0.0.1:1"})
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	rec := httptest.NewRecorder()
	h.listCalendars(rec, calReq("acct", http.MethodGet, "", map[string]string{"accountID": "acct"}))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("upstream error=%d want 502", rec.Code)
	}
}
