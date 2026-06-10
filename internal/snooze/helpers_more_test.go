package snooze

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHandlersMissingAuthContext covers the "missing auth context"
// guard in list/get/wakeNow: with no tenant/kchat on the request
// context the handlers must 500 rather than leak across tenants.
func TestHandlersMissingAuthContext(t *testing.T) {
	h := newHandlersWith(&fakeManager{}, &fakeDispatcher{}, func() time.Time { return testNow })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/snoozed", h.list)
	mux.HandleFunc("GET /api/v1/snoozed/{id}", h.get)
	mux.HandleFunc("DELETE /api/v1/snoozed/{id}", h.wakeNow)
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/snoozed"},
		{http.MethodGet, "/api/v1/snoozed/abc"},
		{http.MethodDelete, "/api/v1/snoozed/abc"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s = %d, want 500 (missing auth)", tc.method, tc.target, rec.Code)
		}
	}
}

func TestFormatNotUpdated(t *testing.T) {
	cases := []struct {
		name   string
		reason any
		want   string
	}{
		{"type+desc", map[string]any{"type": "notFound", "description": "vanished"}, "notFound: vanished"},
		{"type only", map[string]any{"type": "forbidden"}, "forbidden"},
		{"opaque", []any{1, 2}, "[1,2]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatNotUpdated(c.reason); !strings.Contains(got.Error(), c.want) {
				t.Errorf("formatNotUpdated(%v)=%q want substring %q", c.reason, got, c.want)
			}
		})
	}
}

func TestToResponseWokenAt(t *testing.T) {
	woken := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r := toResponse(&Snooze{ID: "s1", Status: StatusUnsnoozed, EmailID: "e1", WokenAt: &woken})
	if r.WokenAt == nil || !strings.HasPrefix(*r.WokenAt, "2026-01-02T03:04:05") {
		t.Fatalf("WokenAt not formatted: %+v", r.WokenAt)
	}
	// nil WokenAt → omitted.
	if toResponse(&Snooze{ID: "s2"}).WokenAt != nil {
		t.Error("nil WokenAt should map to nil")
	}
}

// TestStoreMethodsNoOpDB covers the RowsAffected==0 branch of the
// worker store mutators: a non-existent id is a logged no-op (not an
// error) because a user Cancel can race the worker's terminal write.
func TestStoreMethodsNoOpDB(t *testing.T) {
	svc, _ := dbService(t, time.Now)
	ctx := context.Background()
	missing := "00000000-0000-0000-0000-000000000000"

	if err := svc.markUnsnoozed(ctx, missing, time.Now()); err != nil {
		t.Errorf("markUnsnoozed no-op should not error, got %v", err)
	}
	if err := svc.markFailed(ctx, missing, "boom"); err != nil {
		t.Errorf("markFailed no-op should not error, got %v", err)
	}
	if err := svc.scheduleRetry(ctx, missing, time.Now().Add(time.Minute), "boom"); err != nil {
		t.Errorf("scheduleRetry no-op should not error, got %v", err)
	}
}
