package deliverability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func dbHandlers(t *testing.T) (*Handlers, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(Config{Pool: pool, Logger: log.New(io.Discard, "", 0)})
	return NewHandlers(svc, log.New(io.Discard, "", 0)), tenant
}

func scoped(tenant, method, body string, pv map[string]string) *http.Request {
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

func TestDeliverabilitySuppressionHandlersDB(t *testing.T) {
	h, tenant := dbHandlers(t)
	idv := map[string]string{"id": tenant}

	rec := httptest.NewRecorder()
	h.addSuppression(rec, scoped(tenant, http.MethodPost, `{"email":"bad@example.com","reason":"hard_bounce","source":"test"}`, idv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("addSuppression=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.listSuppression(rec, scoped(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Fatalf("listSuppression=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.removeSuppression(rec, scoped(tenant, http.MethodDelete, "", map[string]string{"id": tenant, "email": "bad@example.com"}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("removeSuppression=%d body=%s", rec.Code, rec.Body.String())
	}

	// Cross-tenant scope rejected.
	rec = httptest.NewRecorder()
	h.listSuppression(rec, scoped("00000000-0000-0000-0000-000000000000", http.MethodGet, "", idv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-tenant=%d want 403", rec.Code)
	}
}

func TestDeliverabilityBounceHandlersDB(t *testing.T) {
	h, tenant := dbHandlers(t)
	idv := map[string]string{"id": tenant}

	rec := httptest.NewRecorder()
	h.recordBounce(rec, scoped(tenant, http.MethodPost,
		`{"email":"x@example.com","bounce_type":"hard","dsn_code":"5.1.1","diagnostic":"no such user"}`, idv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("recordBounce=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.listBounces(rec, scoped(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Errorf("listBounces=%d", rec.Code)
	}
}

func TestDeliverabilityIPPoolHandlersDB(t *testing.T) {
	h, tenant := dbHandlers(t)
	u := fmt.Sprintf("%d", time.Now().UnixNano())

	// Create a global pool (admin route, no tenant scope).
	rec := httptest.NewRecorder()
	h.createPool(rec, scoped(tenant, http.MethodPost,
		`{"name":"pool-`+u+`","pool_type":"mature_trusted","description":"d"}`, nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("createPool=%d body=%s", rec.Code, rec.Body.String())
	}
	var pool struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pool)

	rec = httptest.NewRecorder()
	h.listPools(rec, scoped(tenant, http.MethodGet, "", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("listPools=%d", rec.Code)
	}

	// Add an IP with a unique address.
	rec = httptest.NewRecorder()
	h.addIP(rec, scoped(tenant, http.MethodPost,
		`{"address":"203.0.`+fmt.Sprintf("%d.%d", time.Now().UnixNano()%250+1, time.Now().UnixNano()%200+1)+`","reverse_dns":"mx.example.com"}`,
		map[string]string{"id": pool.ID}))
	if rec.Code != http.StatusCreated {
		t.Logf("addIP=%d body=%s (address collisions are acceptable)", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.listIPs(rec, scoped(tenant, http.MethodGet, "", map[string]string{"id": pool.ID}))
	if rec.Code != http.StatusOK {
		t.Errorf("listIPs=%d", rec.Code)
	}

	// Assign tenant pool + get.
	idv := map[string]string{"id": tenant}
	rec = httptest.NewRecorder()
	h.assignTenantPool(rec, scoped(tenant, http.MethodPost, `{"pool_type":"mature_trusted","priority":1}`, idv))
	if rec.Code != http.StatusNoContent {
		t.Errorf("assignTenantPool=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.getTenantPool(rec, scoped(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Errorf("getTenantPool=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeliverabilitySendLimitAndWarmupHandlersDB(t *testing.T) {
	h, tenant := dbHandlers(t)
	idv := map[string]string{"id": tenant}

	rec := httptest.NewRecorder()
	h.setSendLimit(rec, scoped(tenant, http.MethodPatch, `{"daily_limit":1000,"hourly_limit":100}`, idv))
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("setSendLimit=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.getSendLimit(rec, scoped(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Errorf("getSendLimit=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.getWarmup(rec, scoped(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Errorf("getWarmup=%d body=%s", rec.Code, rec.Body.String())
	}
}

const sampleDMARCReport = `<?xml version="1.0"?>
<feedback>
  <report_metadata>
    <org_name>google.com</org_name>
    <email>noreply-dmarc@google.com</email>
    <report_id>rpt-1</report_id>
    <date_range><begin>1700000000</begin><end>1700086400</end></date_range>
  </report_metadata>
  <policy_published>
    <domain>example.com</domain>
    <adkim>r</adkim><aspf>r</aspf><p>none</p>
  </policy_published>
  <record>
    <row>
      <source_ip>203.0.113.10</source_ip>
      <count>5</count>
      <policy_evaluated><disposition>none</disposition><dkim>pass</dkim><spf>pass</spf></policy_evaluated>
    </row>
    <identifiers><header_from>example.com</header_from></identifiers>
    <auth_results><dkim><domain>example.com</domain><result>pass</result></dkim></auth_results>
  </record>
</feedback>`

func TestDeliverabilityDMARCHandlersDB(t *testing.T) {
	h, tenant := dbHandlers(t)
	idv := map[string]string{"id": tenant}

	rec := httptest.NewRecorder()
	h.uploadDMARC(rec, scoped(tenant, http.MethodPost, sampleDMARCReport, idv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("uploadDMARC=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.listDMARC(rec, scoped(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Errorf("listDMARC=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.dmarcSummary(rec, scoped(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Errorf("dmarcSummary=%d body=%s", rec.Code, rec.Body.String())
	}

	// Malformed XML → 400.
	rec = httptest.NewRecorder()
	h.uploadDMARC(rec, scoped(tenant, http.MethodPost, `<not-dmarc/>`, idv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad uploadDMARC=%d want 400", rec.Code)
	}
}
