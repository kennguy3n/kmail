package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandlersWithLifecycleDB drives the proration-preview, history and
// change-plan endpoints through the lifecycle-wired Handlers so the
// non-nil-lifecycle branches in handlers.go execute against a real DB.
func TestHandlersWithLifecycleDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()
	lc := NewLifecycle(svc, nil)
	h := NewHandlers(svc, nil).WithLifecycle(lc)

	if err := lc.OnTenantCreated(ctx, tenant, PlanCore); err != nil {
		t.Fatalf("OnTenantCreated: %v", err)
	}

	// proration-preview with a real lifecycle returns the tenant/new_plan
	// payload (cents computed from the subscription period).
	rec := httptest.NewRecorder()
	r := scopedReq(ctx, tenant, http.MethodGet, "")
	r.URL.RawQuery = "plan=" + PlanPro
	h.prorationPreview(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("prorationPreview code=%d body=%s", rec.Code, rec.Body.String())
	}
	var pp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pp); err != nil {
		t.Fatalf("decode proration: %v", err)
	}
	if pp["new_plan"] != PlanPro {
		t.Errorf("proration new_plan=%v want pro", pp["new_plan"])
	}

	// change-plan core → pro records a plan_changed event and (via the
	// lifecycle hook) a plan_prorated event.
	rec = httptest.NewRecorder()
	h.changePlan(rec, scopedReq(ctx, tenant, http.MethodPatch, `{"plan":"pro"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("changePlan code=%d body=%s", rec.Code, rec.Body.String())
	}

	// history now returns the recorded billing events.
	rec = httptest.NewRecorder()
	h.history(rec, scopedReq(ctx, tenant, http.MethodGet, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("history code=%d body=%s", rec.Code, rec.Body.String())
	}
	var hist []BillingHistoryEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(hist) == 0 {
		t.Error("expected at least one billing history entry")
	}
}
