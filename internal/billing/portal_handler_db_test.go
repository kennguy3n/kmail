package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newPortalMockStripe(t *testing.T) *StripeClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/billing_portal/sessions" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "bps_test", "url": "https://billing.stripe.test/session"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	c := NewStripeClient("sk_test_portal")
	c.BaseURL = srv.URL
	return c
}

func TestPortalHandlerCreateDB(t *testing.T) {
	svc, pool, tenant := dbService(t)
	ctx := context.Background()
	lc := NewLifecycle(svc, nil)
	if err := lc.OnTenantCreated(ctx, tenant, PlanPro); err != nil {
		t.Fatalf("OnTenantCreated: %v", err)
	}

	stripe := newPortalMockStripe(t)
	h := NewPortalHandlers(pool, stripe, nil)

	// No stripe_customer_id yet → 404.
	rec := httptest.NewRecorder()
	h.create(rec, scopedReq(ctx, tenant, http.MethodPost, `{"return_url":"https://app.kmail.test/billing"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no-customer code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Attach a stripe customer id, then a portal session is minted.
	if _, err := pool.Exec(ctx, `UPDATE billing_subscriptions SET stripe_customer_id = 'cus_portal' WHERE tenant_id = $1::uuid`, tenant); err != nil {
		t.Fatalf("set customer id: %v", err)
	}
	rec = httptest.NewRecorder()
	h.create(rec, scopedReq(ctx, tenant, http.MethodPost, `{"return_url":"https://app.kmail.test/billing"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("portal create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out PortalSessionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.URL == "" {
		t.Fatalf("portal session body=%s err=%v", rec.Body.String(), err)
	}
}

func TestPortalHandlerGuards(t *testing.T) {
	ctx := context.Background()

	// Cross-tenant access is forbidden.
	h := NewPortalHandlers(nil, nil, nil)
	r := scopedReq(ctx, "tenant-a", http.MethodPost, `{}`)
	r.SetPathValue("id", "tenant-b") // path tenant differs from ctx tenant
	rec := httptest.NewRecorder()
	h.create(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-tenant code=%d want 403", rec.Code)
	}

	// Stripe not configured → 503.
	h = NewPortalHandlers(nil, NewStripeClient(""), nil)
	rec = httptest.NewRecorder()
	h.create(rec, scopedReq(ctx, "tenant-a", http.MethodPost, `{}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured stripe code=%d want 503", rec.Code)
	}
}
