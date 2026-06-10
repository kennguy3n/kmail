package deliverability

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPhase3ScopedHandlers_CrossTenant asserts every tenant-scoped
// phase-3 handler rejects a cross-tenant request with 403.
func TestPhase3ScopedHandlers_CrossTenant(t *testing.T) {
	h, tenant := dbHandlers(t)
	other := "00000000-0000-0000-0000-000000000000"
	pv := map[string]string{"id": tenant}

	handlers := []struct {
		name   string
		fn     http.HandlerFunc
		method string
	}{
		{"ingestGmailPostmaster", h.ingestGmailPostmaster, http.MethodPost},
		{"listFeedback", h.listFeedback, http.MethodGet},
		{"feedbackSummary", h.feedbackSummary, http.MethodGet},
		{"abuseScore", h.abuseScore, http.MethodGet},
		{"listAbuseAlerts", h.listAbuseAlerts, http.MethodGet},
		{"listDeliverabilityAlerts", h.listDeliverabilityAlerts, http.MethodGet},
		{"listThresholds", h.listThresholds, http.MethodGet},
		{"setThresholds", h.setThresholds, http.MethodPut},
	}
	for _, hc := range handlers {
		rec := httptest.NewRecorder()
		hc.fn(rec, scoped(other, hc.method, "", pv))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s cross-tenant=%d want 403", hc.name, rec.Code)
		}
	}
}

// TestPhase3List_AcknowledgedFilter covers the acknowledged query
// parameter branch on the alert listing handlers.
func TestPhase3List_AcknowledgedFilter(t *testing.T) {
	h, tenant := dbHandlers(t)
	pv := map[string]string{"id": tenant}

	for _, fn := range []http.HandlerFunc{h.listAbuseAlerts, h.listDeliverabilityAlerts} {
		rec := httptest.NewRecorder()
		r := scoped(tenant, http.MethodGet, "", pv)
		r.URL.RawQuery = "acknowledged=true&severity=critical&limit=5&offset=0"
		fn(rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("list with acknowledged filter=%d want 200 body=%s", rec.Code, rec.Body.String())
		}
	}
}

// TestPhase3IPReputation_History covers the admin IP-reputation
// handlers' filter branches.
func TestPhase3IPReputation_Filters(t *testing.T) {
	h, _ := dbHandlers(t)

	// ipReputation with a pool_type filter.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.URL.RawQuery = "pool_type=shared&status=active"
	h.ipReputation(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("ipReputation filtered=%d body=%s", rec.Code, rec.Body.String())
	}
}
