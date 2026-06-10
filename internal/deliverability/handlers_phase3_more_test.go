package deliverability

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// TestRegisterPhase3 asserts the route table mounts without panic
// and that a wrapped route is reachable end-to-end through the mux
// (dev-bypass auth) — covering RegisterPhase3 and the mux wiring.
func TestRegisterPhase3(t *testing.T) {
	h, tenant := dbHandlers(t)
	authMW := middleware.MustNewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-secret",
		Env:            middleware.EnvDevelopment,
	})
	mux := http.NewServeMux()
	h.RegisterPhase3(mux, authMW)

	// The route is mounted (not 404). A 401 from the auth wrapper
	// still proves RegisterPhase3 wired the handler onto the mux.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/"+tenant+"/deliverability/thresholds", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("listThresholds route not mounted (404)")
	}

	// With dev-bypass auth + matching dev tenant header the request
	// flows all the way through to the handler and returns 200.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tenants/"+tenant+"/deliverability/thresholds", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("routed listThresholds=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPhase3AckHandlers(t *testing.T) {
	h, tenant := dbHandlers(t)
	other := "00000000-0000-0000-0000-000000000000"

	// ackAbuseAlert cross-tenant ⇒ 403.
	rec := httptest.NewRecorder()
	h.ackAbuseAlert(rec, scoped(other, http.MethodPost, "", map[string]string{"id": tenant, "alertId": "x"}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("ackAbuseAlert cross-tenant=%d want 403", rec.Code)
	}

	// ackAbuseAlert non-existent alert ⇒ 404 (ErrNotFound via statusFor).
	rec = httptest.NewRecorder()
	h.ackAbuseAlert(rec, scoped(tenant, http.MethodPost, "", map[string]string{"id": tenant, "alertId": other}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("ackAbuseAlert missing=%d want 404 body=%s", rec.Code, rec.Body.String())
	}

	// ackDeliverabilityAlert cross-tenant ⇒ 403.
	rec = httptest.NewRecorder()
	h.ackDeliverabilityAlert(rec, scoped(other, http.MethodPost, "", map[string]string{"id": tenant, "alertId": "x"}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("ackDeliverabilityAlert cross-tenant=%d want 403", rec.Code)
	}

	// ackDeliverabilityAlert non-existent ⇒ 404.
	rec = httptest.NewRecorder()
	h.ackDeliverabilityAlert(rec, scoped(tenant, http.MethodPost, "", map[string]string{"id": tenant, "alertId": other}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("ackDeliverabilityAlert missing=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

func TestPhase3IngestYahooARF(t *testing.T) {
	h, tenant := dbHandlers(t)
	pv := map[string]string{"id": tenant}

	// JSON body path → 201.
	rec := httptest.NewRecorder()
	body := `{"original_rcpt_to":"victim@example.com","source_ip":"203.0.113.7","feedback_type":"abuse"}`
	h.ingestYahooARF(rec, scoped(tenant, http.MethodPost, body, pv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingestYahooARF json=%d body=%s", rec.Code, rec.Body.String())
	}

	// ARF (message/feedback-report) content-type path → 201.
	arf := "Feedback-Type: abuse\r\nUser-Agent: test\r\nVersion: 1\r\nOriginal-Rcpt-To: v2@example.com\r\nSource-IP: 203.0.113.8\r\n"
	rec = httptest.NewRecorder()
	r := scoped(tenant, http.MethodPost, arf, pv)
	r.Header.Set("Content-Type", "message/feedback-report")
	h.ingestYahooARF(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingestYahooARF arf=%d body=%s", rec.Code, rec.Body.String())
	}

	// Malformed JSON → 400.
	rec = httptest.NewRecorder()
	h.ingestYahooARF(rec, scoped(tenant, http.MethodPost, `{bad`, pv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ingestYahooARF bad-json=%d want 400", rec.Code)
	}

	// Cross-tenant → 403.
	rec = httptest.NewRecorder()
	h.ingestYahooARF(rec, scoped("00000000-0000-0000-0000-000000000000", http.MethodPost, body, pv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("ingestYahooARF cross-tenant=%d want 403", rec.Code)
	}
}

func TestPhase3IPReputationHistory(t *testing.T) {
	h, _ := dbHandlers(t)
	// Admin route (no tenant scope). Unknown IP id ⇒ handler still
	// responds (empty history or 404) without panicking.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.SetPathValue("ipId", "00000000-0000-0000-0000-000000000000")
	h.ipReputationHistory(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Fatalf("ipReputationHistory=%d body=%s", rec.Code, rec.Body.String())
	}
}
