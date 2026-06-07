package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

func scopedReq(ctx context.Context, tenant, method, body string) *http.Request {
	ctx = middleware.WithTenantID(ctx, tenant)
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(ctx, method, "/x", nil)
	} else {
		r = httptest.NewRequestWithContext(ctx, method, "/x", strings.NewReader(body))
	}
	r.SetPathValue("id", tenant)
	return r
}

func TestBillingHandlersDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	lc := NewLifecycle(svc, nil)
	h := NewHandlers(svc, nil).WithLifecycle(lc)
	ctx := context.Background()

	if err := lc.OnTenantCreated(ctx, tenant, PlanPro); err != nil {
		t.Fatalf("OnTenantCreated: %v", err)
	}

	// summary
	rec := httptest.NewRecorder()
	h.summary(rec, scopedReq(ctx, tenant, http.MethodGet, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sum BillingSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil || sum.Plan != PlanPro {
		t.Fatalf("summary body=%s err=%v", rec.Body.String(), err)
	}

	// usage
	rec = httptest.NewRecorder()
	h.usage(rec, scopedReq(ctx, tenant, http.MethodGet, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("usage status=%d", rec.Code)
	}

	// updateLimits
	rec = httptest.NewRecorder()
	h.updateLimits(rec, scopedReq(ctx, tenant, http.MethodPatch, `{"seat_limit":20}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("updateLimits status=%d body=%s", rec.Code, rec.Body.String())
	}

	// changePlan pro -> privacy
	rec = httptest.NewRecorder()
	h.changePlan(rec, scopedReq(ctx, tenant, http.MethodPatch, `{"plan":"privacy"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("changePlan status=%d body=%s", rec.Code, rec.Body.String())
	}

	// invoice
	rec = httptest.NewRecorder()
	h.invoice(rec, scopedReq(ctx, tenant, http.MethodPost, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("invoice status=%d", rec.Code)
	}

	// proration preview
	rec = httptest.NewRecorder()
	req := scopedReq(ctx, tenant, http.MethodGet, "")
	req.URL.RawQuery = "plan=core"
	h.prorationPreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proration status=%d body=%s", rec.Code, rec.Body.String())
	}

	// history
	rec = httptest.NewRecorder()
	h.history(rec, scopedReq(ctx, tenant, http.MethodGet, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("history status=%d", rec.Code)
	}
}

func TestBillingHandlersScopeAndValidation(t *testing.T) {
	h := NewHandlers(NewService(Config{}), nil)
	ctx := context.Background()

	// Missing tenant context → 403.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/x", nil)
	req.SetPathValue("id", "tenant-a")
	h.summary(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no-scope summary status=%d want 403", rec.Code)
	}

	// Cross-tenant mismatch → 403.
	rec = httptest.NewRecorder()
	cctx := middleware.WithTenantID(ctx, "tenant-a")
	req = httptest.NewRequestWithContext(cctx, http.MethodGet, "/x", nil)
	req.SetPathValue("id", "tenant-b")
	h.summary(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-tenant status=%d want 403", rec.Code)
	}

	// Invalid plan on proration preview → 400.
	rec = httptest.NewRecorder()
	req = scopedReq(ctx, "tenant-a", http.MethodGet, "")
	req.URL.RawQuery = "plan=bogus"
	h.prorationPreview(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad plan status=%d want 400", rec.Code)
	}
}

func TestStatusForMapping(t *testing.T) {
	cases := map[error]int{
		ErrInvalidInput:  http.StatusBadRequest,
		ErrNotFound:      http.StatusNotFound,
		ErrQuotaExceeded: http.StatusPaymentRequired,
	}
	for err, want := range cases {
		if got := statusFor(err); got != want {
			t.Errorf("statusFor(%v)=%d want %d", err, got, want)
		}
	}
}
