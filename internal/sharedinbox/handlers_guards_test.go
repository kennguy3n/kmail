package sharedinbox

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// noTenantReq builds a request with NO tenant context so the
// tenantFromReq guard fires.
func noTenantReq(method, body string, pv map[string]string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/x", nil)
	} else {
		r = httptest.NewRequest(method, "/x", strings.NewReader(body))
	}
	for k, v := range pv {
		r.SetPathValue(k, v)
	}
	return r
}

// TestHandlersForbiddenWithoutTenant covers the 403 guard branch on
// every handler that requires tenant context.
func TestHandlersForbiddenWithoutTenant(t *testing.T) {
	h := NewHandlers(NewService(nil, log.New(io.Discard, "", 0)), log.New(io.Discard, "", 0))
	pv := map[string]string{"inboxId": "i", "emailId": "e"}

	checks := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{"assign", h.assign, noTenantReq(http.MethodPost, `{"assignee_user_id":"u"}`, pv)},
		{"unassign", h.unassign, noTenantReq(http.MethodDelete, "", pv)},
		{"setStatus", h.setStatus, noTenantReq(http.MethodPut, `{"status":"open"}`, pv)},
		{"addNote", h.addNote, noTenantReq(http.MethodPost, `{"note_text":"x"}`, pv)},
		{"listNotes", h.listNotes, noTenantReq(http.MethodGet, "", pv)},
		{"mlsStatus", h.mlsStatus, noTenantReq(http.MethodGet, "", pv)},
	}
	for _, c := range checks {
		rec := httptest.NewRecorder()
		c.fn(rec, c.req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without tenant = %d, want 403", c.name, rec.Code)
		}
	}
}

// TestUnassignHandlerNotFound drives the ErrNotFound → 404 path through
// the handler so statusFor's not-found branch is covered.
func TestUnassignHandlerNotFound(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(pool, log.New(io.Discard, "", 0))
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	inbox, _ := seedInboxAndUser(t, svc, tenant)

	rec := httptest.NewRecorder()
	h.unassign(rec, siReq(tenant, http.MethodDelete, "", map[string]string{"inboxId": inbox, "emailId": "missing"}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unassign missing = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

// TestStatusForDefault pins the 500 fallback for unclassified errors.
func TestStatusForDefault(t *testing.T) {
	if got := statusFor(errors.New("opaque")); got != http.StatusInternalServerError {
		t.Errorf("statusFor(opaque) = %d want 500", got)
	}
	if got := statusFor(context.Canceled); got != http.StatusInternalServerError {
		t.Errorf("statusFor(canceled) = %d want 500", got)
	}
}
