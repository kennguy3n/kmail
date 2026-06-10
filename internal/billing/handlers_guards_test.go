package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// crossTenantReq builds a request whose ctx tenant differs from the
// path tenant so checkTenantScope rejects it.
func crossTenantReq(method string) *http.Request {
	r := scopedReq(context.Background(), "tenant-ctx", method, `{}`)
	r.SetPathValue("id", "tenant-path")
	return r
}

func TestHandlersForbidCrossTenant(t *testing.T) {
	h := NewHandlers(NewService(Config{}), nil).WithLifecycle(NewLifecycle(NewService(Config{}), nil))
	fns := map[string]http.HandlerFunc{
		"summary":          h.summary,
		"usage":            h.usage,
		"updateLimits":     h.updateLimits,
		"changePlan":       h.changePlan,
		"invoice":          h.invoice,
		"prorationPreview": h.prorationPreview,
		"history":          h.history,
	}
	for name, fn := range fns {
		rec := httptest.NewRecorder()
		fn(rec, crossTenantReq(http.MethodGet))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s cross-tenant code=%d want 403", name, rec.Code)
		}
	}

	// Missing tenant context is also forbidden.
	rec := httptest.NewRecorder()
	h.summary(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing ctx code=%d want 403", rec.Code)
	}
}

func TestHandlersBadInput(t *testing.T) {
	h := NewHandlers(NewService(Config{}), nil)
	ctx := context.Background()

	// prorationPreview with an invalid plan → 400.
	rec := httptest.NewRecorder()
	r := scopedReq(ctx, "t1", http.MethodGet, "")
	q := r.URL.Query()
	q.Set("plan", "not-a-plan")
	r.URL.RawQuery = q.Encode()
	h.prorationPreview(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad plan code=%d want 400", rec.Code)
	}

	// changePlan with malformed JSON → 400.
	rec = httptest.NewRecorder()
	h.changePlan(rec, scopedReq(ctx, "t1", http.MethodPatch, "{not json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("changePlan bad json code=%d want 400", rec.Code)
	}

	// updateLimits with malformed JSON → 400.
	rec = httptest.NewRecorder()
	h.updateLimits(rec, scopedReq(ctx, "t1", http.MethodPatch, "{not json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("updateLimits bad json code=%d want 400", rec.Code)
	}
}

func TestHandlersNilLifecycleFallback(t *testing.T) {
	// With no lifecycle wired, proration-preview and history return
	// empty/zero shells rather than erroring.
	h := NewHandlers(NewService(Config{}), nil)
	ctx := context.Background()

	rec := httptest.NewRecorder()
	r := scopedReq(ctx, "t1", http.MethodGet, "")
	q := r.URL.Query()
	q.Set("plan", PlanPro)
	r.URL.RawQuery = q.Encode()
	h.prorationPreview(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("nil-lifecycle proration code=%d want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.history(rec, scopedReq(ctx, "t1", http.MethodGet, ""))
	if rec.Code != http.StatusOK {
		t.Errorf("nil-lifecycle history code=%d want 200", rec.Code)
	}
}
