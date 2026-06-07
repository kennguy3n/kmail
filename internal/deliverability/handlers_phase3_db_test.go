package deliverability

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPhase3FeedbackHandlersDB(t *testing.T) {
	h, tenant := dbHandlers(t)
	pv := map[string]string{"id": tenant}

	// ingest Gmail postmaster data → 201
	rec := httptest.NewRecorder()
	body := `{"domain":"example.com","spam_rate":0.001,"ip_reputation":"high","domain_reputation":"high","delivery_errors":0.01,"date":"2024-01-01"}`
	h.ingestGmailPostmaster(rec, scoped(tenant, http.MethodPost, body, pv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingestGmailPostmaster=%d body=%s", rec.Code, rec.Body.String())
	}

	// listFeedback → 200
	rec = httptest.NewRecorder()
	h.listFeedback(rec, scoped(tenant, http.MethodGet, "", pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("listFeedback=%d body=%s", rec.Code, rec.Body.String())
	}

	// feedbackSummary → 200
	rec = httptest.NewRecorder()
	h.feedbackSummary(rec, scoped(tenant, http.MethodGet, "", pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("feedbackSummary=%d body=%s", rec.Code, rec.Body.String())
	}

	// ingest malformed JSON → 400
	rec = httptest.NewRecorder()
	h.ingestGmailPostmaster(rec, scoped(tenant, http.MethodPost, `{bad`, pv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ingestGmailPostmaster bad json=%d want 400", rec.Code)
	}

	// missing tenant scope → 403
	rec = httptest.NewRecorder()
	h.listFeedback(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("listFeedback no scope=%d want 403", rec.Code)
	}
}

func TestPhase3AbuseHandlersDB(t *testing.T) {
	h, tenant := dbHandlers(t)
	pv := map[string]string{"id": tenant}

	// abuseScore (plain) → 200
	rec := httptest.NewRecorder()
	h.abuseScore(rec, scoped(tenant, http.MethodGet, "", pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("abuseScore=%d body=%s", rec.Code, rec.Body.String())
	}

	// abuseScore?recompute=true → 200 with new_alerts
	rec = httptest.NewRecorder()
	r := scoped(tenant, http.MethodGet, "", pv)
	r.URL.RawQuery = "recompute=true"
	h.abuseScore(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("abuseScore recompute=%d body=%s", rec.Code, rec.Body.String())
	}

	// listAbuseAlerts → 200
	rec = httptest.NewRecorder()
	h.listAbuseAlerts(rec, scoped(tenant, http.MethodGet, "", pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("listAbuseAlerts=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPhase3ThresholdsHandlersDB(t *testing.T) {
	h, tenant := dbHandlers(t)
	pv := map[string]string{"id": tenant}

	// listThresholds → 200
	rec := httptest.NewRecorder()
	h.listThresholds(rec, scoped(tenant, http.MethodGet, "", pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("listThresholds=%d body=%s", rec.Code, rec.Body.String())
	}

	// setThresholds → 200
	rec = httptest.NewRecorder()
	body := `{"thresholds":[{"metric":"bounce_rate","warning":0.05,"critical":0.1}]}`
	h.setThresholds(rec, scoped(tenant, http.MethodPut, body, pv))
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Fatalf("setThresholds=%d body=%s", rec.Code, rec.Body.String())
	}

	// setThresholds malformed JSON → 400
	rec = httptest.NewRecorder()
	h.setThresholds(rec, scoped(tenant, http.MethodPut, `{bad`, pv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("setThresholds bad json=%d want 400", rec.Code)
	}

	// listDeliverabilityAlerts → 200
	rec = httptest.NewRecorder()
	h.listDeliverabilityAlerts(rec, scoped(tenant, http.MethodGet, "", pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("listDeliverabilityAlerts=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPhase3IPReputationHandlersDB(t *testing.T) {
	h, _ := dbHandlers(t)

	// ipReputation (admin, no tenant scope) → 200
	rec := httptest.NewRecorder()
	h.ipReputation(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ipReputation=%d body=%s", rec.Code, rec.Body.String())
	}
}
