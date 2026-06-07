package calendarbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func shareReq(tenant, account, method, body string, pv map[string]string) *http.Request {
	ctx := middleware.WithTenantID(context.Background(), tenant)
	ctx = middleware.WithStalwartAccountID(ctx, account)
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

func TestSharingHandlersStoreDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	store := NewSharingStore(pool)
	// svc is nil: the store-backed handlers under test never deref it.
	h := NewSharingHandlers(nil, store)

	// shareCalendar
	rec := httptest.NewRecorder()
	h.shareCalendar(rec, shareReq(tenant, "owner-acct", http.MethodPost,
		`{"target_account_id":"target-acct","permission":"read"}`, map[string]string{"id": "cal-h1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("shareCalendar=%d body=%s", rec.Code, rec.Body.String())
	}

	// listShared (as the target)
	rec = httptest.NewRecorder()
	h.listShared(rec, shareReq(tenant, "target-acct", http.MethodGet, "", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("listShared=%d", rec.Code)
	}
	var shared struct {
		Shares []CalendarShare `json:"shares"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &shared)
	if len(shared.Shares) == 0 {
		t.Errorf("listShared returned no shares")
	}

	// createResource
	rec = httptest.NewRecorder()
	h.createResource(rec, shareReq(tenant, "owner-acct", http.MethodPost,
		`{"name":"Conf Room A","resource_type":"room","location":"HQ","capacity":8}`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("createResource=%d body=%s", rec.Code, rec.Body.String())
	}

	// listResources
	rec = httptest.NewRecorder()
	h.listResources(rec, shareReq(tenant, "owner-acct", http.MethodGet, "", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("listResources=%d", rec.Code)
	}
}

func TestSharingHandlersScopeAndValidation(t *testing.T) {
	pool := testsupport.Pool(t)
	store := NewSharingStore(pool)
	h := NewSharingHandlers(nil, store)

	// Missing tenant context → 403.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	h.listShared(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("listShared no tenant=%d want 403", rec.Code)
	}

	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	// Invalid permission → 400.
	rec = httptest.NewRecorder()
	h.shareCalendar(rec, shareReq(tenant, "owner", http.MethodPost,
		`{"target_account_id":"t","permission":"bogus"}`, map[string]string{"id": "cal"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("shareCalendar bad perm=%d want 400 body=%s", rec.Code, rec.Body.String())
	}

	// Invalid resource_type → 400.
	rec = httptest.NewRecorder()
	h.createResource(rec, shareReq(tenant, "owner", http.MethodPost,
		`{"name":"X","resource_type":"bogus"}`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createResource bad type=%d want 400", rec.Code)
	}

	// Malformed JSON → 400.
	rec = httptest.NewRecorder()
	h.createResource(rec, shareReq(tenant, "owner", http.MethodPost, `{bad`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createResource bad json=%d want 400", rec.Code)
	}
}
