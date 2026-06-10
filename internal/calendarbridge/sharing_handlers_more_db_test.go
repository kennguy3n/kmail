package calendarbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// sharingFixture builds SharingHandlers backed by a DB pool + a fake
// Stalwart server, and returns the handlers plus a seeded tenant and
// the principal account id.
func sharingFixture(t *testing.T) (*SharingHandlers, string, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, sampleEventsReportResponse)
		case http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM calendar_shares WHERE tenant_id=$1::uuid`, tenant)
		_, _ = pool.Exec(context.Background(), `DELETE FROM resource_calendars WHERE tenant_id=$1::uuid`, tenant)
	})
	svc := NewService(Config{StalwartURL: srv.URL})
	store := NewSharingStore(pool)
	return NewSharingHandlers(svc, store), tenant, "alice"
}

// authed builds a request whose context carries the tenant + stalwart
// principal, mirroring what the OIDC middleware injects.
func authed(tenant, account, method, body string, pv map[string]string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/x", strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "/x", nil)
	}
	ctx := middleware.WithTenantID(r.Context(), tenant)
	ctx = middleware.WithStalwartAccountID(ctx, account)
	r = r.WithContext(ctx)
	for k, v := range pv {
		r.SetPathValue(k, v)
	}
	return r
}

func TestSharingHandlers_CalendarCRUD(t *testing.T) {
	h, tenant, account := sharingFixture(t)

	// createCalendar happy.
	rec := httptest.NewRecorder()
	h.createCalendar(rec, authed(tenant, account, http.MethodPost, `{"name":"Team","calendar_type":"shared"}`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("createCalendar=%d body=%s", rec.Code, rec.Body.String())
	}
	var cal Calendar
	_ = json.Unmarshal(rec.Body.Bytes(), &cal)
	if cal.ID == "" {
		t.Errorf("createCalendar empty id")
	}

	// createCalendar bad JSON ⇒ 400.
	rec = httptest.NewRecorder()
	h.createCalendar(rec, authed(tenant, account, http.MethodPost, `{bad`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createCalendar bad-json=%d want 400", rec.Code)
	}

	// createCalendar invalid type ⇒ 400 (statusFromErr ErrInvalidInput).
	rec = httptest.NewRecorder()
	h.createCalendar(rec, authed(tenant, account, http.MethodPost, `{"name":"X","calendar_type":"bogus"}`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createCalendar bad-type=%d want 400", rec.Code)
	}

	// updateCalendar happy.
	rec = httptest.NewRecorder()
	h.updateCalendar(rec, authed(tenant, account, http.MethodPut, `{"name":"Renamed"}`, map[string]string{"id": "team"}))
	if rec.Code != http.StatusOK {
		t.Errorf("updateCalendar=%d body=%s", rec.Code, rec.Body.String())
	}

	// updateCalendar bad JSON ⇒ 400.
	rec = httptest.NewRecorder()
	h.updateCalendar(rec, authed(tenant, account, http.MethodPut, `{bad`, map[string]string{"id": "team"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("updateCalendar bad-json=%d want 400", rec.Code)
	}

	// deleteCalendar happy ⇒ 204.
	rec = httptest.NewRecorder()
	h.deleteCalendar(rec, authed(tenant, account, http.MethodDelete, "", map[string]string{"id": "team"}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("deleteCalendar=%d want 204", rec.Code)
	}

	// deleteCalendar missing id ⇒ 400 (ErrInvalidInput).
	rec = httptest.NewRecorder()
	h.deleteCalendar(rec, authed(tenant, account, http.MethodDelete, "", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("deleteCalendar no-id=%d want 400", rec.Code)
	}
}

func TestSharingHandlers_ShareAndList(t *testing.T) {
	h, tenant, account := sharingFixture(t)

	// shareCalendar happy.
	rec := httptest.NewRecorder()
	h.shareCalendar(rec, authed(tenant, account, http.MethodPost, `{"target_account_id":"bob","permission":"read"}`, map[string]string{"id": "cal-1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("shareCalendar=%d body=%s", rec.Code, rec.Body.String())
	}

	// shareCalendar bad permission ⇒ 400.
	rec = httptest.NewRecorder()
	h.shareCalendar(rec, authed(tenant, account, http.MethodPost, `{"target_account_id":"bob","permission":"bogus"}`, map[string]string{"id": "cal-1"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("shareCalendar bad-perm=%d want 400", rec.Code)
	}

	// shareCalendar missing tenant ⇒ 403.
	rec = httptest.NewRecorder()
	noTenant := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`))
	noTenant.SetPathValue("id", "cal-1")
	h.shareCalendar(rec, noTenant)
	if rec.Code != http.StatusForbidden {
		t.Errorf("shareCalendar no-tenant=%d want 403", rec.Code)
	}

	// listShared (bob is the target) returns the share.
	rec = httptest.NewRecorder()
	h.listShared(rec, authed(tenant, "bob", http.MethodGet, "", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cal-1") {
		t.Errorf("listShared=%d body=%s", rec.Code, rec.Body.String())
	}

	// listShared missing tenant ⇒ 403.
	rec = httptest.NewRecorder()
	h.listShared(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("listShared no-tenant=%d want 403", rec.Code)
	}
}

func TestSharingHandlers_Resources(t *testing.T) {
	h, tenant, account := sharingFixture(t)

	// createResource happy.
	rec := httptest.NewRecorder()
	h.createResource(rec, authed(tenant, account, http.MethodPost, `{"name":"Boardroom","resource_type":"room","location":"HQ","capacity":10,"caldav_id":"res-1"}`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("createResource=%d body=%s", rec.Code, rec.Body.String())
	}

	// createResource invalid type ⇒ 400.
	rec = httptest.NewRecorder()
	h.createResource(rec, authed(tenant, account, http.MethodPost, `{"name":"X","resource_type":"bogus"}`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createResource bad-type=%d want 400", rec.Code)
	}

	// createResource bad JSON ⇒ 400.
	rec = httptest.NewRecorder()
	h.createResource(rec, authed(tenant, account, http.MethodPost, `{bad`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createResource bad-json=%d want 400", rec.Code)
	}

	// createResource missing tenant ⇒ 403.
	rec = httptest.NewRecorder()
	h.createResource(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`)))
	if rec.Code != http.StatusForbidden {
		t.Errorf("createResource no-tenant=%d want 403", rec.Code)
	}

	// listResources happy.
	rec = httptest.NewRecorder()
	h.listResources(rec, authed(tenant, account, http.MethodGet, "", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Boardroom") {
		t.Errorf("listResources=%d body=%s", rec.Code, rec.Body.String())
	}

	// listResources missing tenant ⇒ 403.
	rec = httptest.NewRecorder()
	h.listResources(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("listResources no-tenant=%d want 403", rec.Code)
	}
}

func TestSharingHandlers_BookResource(t *testing.T) {
	h, tenant, account := sharingFixture(t)

	// Non-overlapping window books successfully (the fake REPORT
	// returns an event whose iCal-basic times don't parse as RFC3339
	// and are skipped, so no conflict is detected).
	start := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	end := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)
	body := `{"start":"` + start + `","end":"` + end + `","subject":"Sync"}`
	rec := httptest.NewRecorder()
	h.bookResource(rec, authed(tenant, account, http.MethodPost, body, map[string]string{"id": "res-1"}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "uid") {
		t.Fatalf("bookResource=%d body=%s", rec.Code, rec.Body.String())
	}

	// end before start ⇒ 400.
	bad := `{"start":"` + end + `","end":"` + start + `","subject":"x"}`
	rec = httptest.NewRecorder()
	h.bookResource(rec, authed(tenant, account, http.MethodPost, bad, map[string]string{"id": "res-1"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bookResource end<start=%d want 400", rec.Code)
	}

	// bad JSON ⇒ 400.
	rec = httptest.NewRecorder()
	h.bookResource(rec, authed(tenant, account, http.MethodPost, `{bad`, map[string]string{"id": "res-1"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bookResource bad-json=%d want 400", rec.Code)
	}
}

func TestSharingHandlers_Register(t *testing.T) {
	h, _, _ := sharingFixture(t)
	mux := http.NewServeMux()
	h.Register(mux, passthroughAuth{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/resource-calendars", nil))
	// Route mounted (403 from missing tenant, not 404).
	if rec.Code == http.StatusNotFound {
		t.Errorf("resource-calendars route not mounted")
	}
}
