package calendarbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func chReq(tenant, method, body string, pv map[string]string) *http.Request {
	ctx := middleware.WithTenantID(context.Background(), tenant)
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

func TestChannelHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	resolver := NewDBChannelResolver(pool, "fallback")
	h := NewChannelHandlers(resolver)
	cal := "cal-ch-" + tenant[:8]
	t.Cleanup(func() {
		_ = resolver.DeleteCalendarChannel(context.Background(), tenant, cal)
		_ = resolver.DeleteCalendarChannel(context.Background(), tenant, "")
	})

	// get with no mapping → configured:false
	rec := httptest.NewRecorder()
	h.get(rec, chReq(tenant, http.MethodGet, "", map[string]string{"calendarId": cal}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("get empty=%d body=%s", rec.Code, rec.Body.String())
	}

	// put a per-calendar channel
	rec = httptest.NewRecorder()
	h.put(rec, chReq(tenant, http.MethodPut, `{"channel_id":"chan-1"}`, map[string]string{"calendarId": cal}))
	if rec.Code != http.StatusOK {
		t.Fatalf("put=%d body=%s", rec.Code, rec.Body.String())
	}

	// get now returns the mapping
	rec = httptest.NewRecorder()
	h.get(rec, chReq(tenant, http.MethodGet, "", map[string]string{"calendarId": cal}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "chan-1") {
		t.Errorf("get configured=%d body=%s", rec.Code, rec.Body.String())
	}

	// tenant default channel via getDefault/putDefault
	rec = httptest.NewRecorder()
	h.putDefault(rec, chReq(tenant, http.MethodPut, `{"channel_id":"default-chan"}`, map[string]string{"id": tenant}))
	if rec.Code != http.StatusOK {
		t.Fatalf("putDefault=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.getDefault(rec, chReq(tenant, http.MethodGet, "", map[string]string{"id": tenant}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "default-chan") {
		t.Errorf("getDefault=%d body=%s", rec.Code, rec.Body.String())
	}

	// delete the per-calendar mapping
	rec = httptest.NewRecorder()
	h.del(rec, chReq(tenant, http.MethodDelete, "", map[string]string{"calendarId": cal}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("del=%d body=%s", rec.Code, rec.Body.String())
	}

	// malformed JSON on put → 400
	rec = httptest.NewRecorder()
	h.put(rec, chReq(tenant, http.MethodPut, `{bad`, map[string]string{"calendarId": cal}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("put bad json=%d want 400", rec.Code)
	}
}
