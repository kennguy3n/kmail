package dns

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func dnsReq(tenant, method, body string, pv map[string]string) *http.Request {
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

func TestDNSHandlersScopeAndRecordsDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	dkimSvc := NewDKIMRotationService(pool, nil)
	domainID := seedDomain(t, dkimSvc, tenant)

	svc := NewService(Config{Pool: pool, MailHost: "mx.kmail.test", SPFInclude: "kmail.test"})
	h := NewHandlers(svc, log.New(io.Discard, "", 0))

	// Cross-tenant request → 403.
	rec := httptest.NewRecorder()
	h.getDomainRecords(rec, dnsReq("00000000-0000-0000-0000-000000000000", http.MethodGet, "",
		map[string]string{"id": tenant, "domainId": domainID}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-tenant getDomainRecords=%d want 403", rec.Code)
	}

	// In-scope getDomainRecords → 200 with the expected record set.
	rec = httptest.NewRecorder()
	h.getDomainRecords(rec, dnsReq(tenant, http.MethodGet, "",
		map[string]string{"id": tenant, "domainId": domainID}))
	if rec.Code != http.StatusOK {
		t.Fatalf("getDomainRecords=%d body=%s", rec.Code, rec.Body.String())
	}

	// Missing tenant context → 403 (missing tenant context branch).
	rec = httptest.NewRecorder()
	noCtx := httptest.NewRequest(http.MethodGet, "/x", nil)
	noCtx.SetPathValue("id", tenant)
	noCtx.SetPathValue("domainId", domainID)
	h.getDomainRecords(rec, noCtx)
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing tenant ctx=%d want 403", rec.Code)
	}

	// Nil-service handler → 501.
	nilH := NewHandlers(nil, log.New(io.Discard, "", 0))
	rec = httptest.NewRecorder()
	nilH.verifyDomain(rec, dnsReq(tenant, http.MethodPost, "", map[string]string{"id": tenant, "domainId": domainID}))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("nil-svc verifyDomain=%d want 501", rec.Code)
	}
}

func TestDNSRegisterMountsDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(Config{Pool: pool})
	authMW := middleware.MustNewOIDC(middleware.OIDCConfig{DevBypassToken: "x", Env: middleware.EnvDevelopment})
	mux := http.NewServeMux()
	NewHandlers(svc, nil).Register(mux, authMW)
	NewDKIMHandlers(NewDKIMRotationService(pool, nil), nil).Register(mux, authMW)
	for _, p := range []string{
		"/api/v1/tenants/t1/domains/d1/dns-records",
		"/api/v1/tenants/t1/domains/d1/dkim",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("route %s not mounted (404)", p)
		}
	}
}

func TestDKIMHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewDKIMRotationService(pool, nil)
	domainID := seedDomain(t, svc, tenant)
	h := NewDKIMHandlers(svc, log.New(io.Discard, "", 0))
	pv := map[string]string{"id": tenant, "domainId": domainID}

	// rotate → 201.
	rec := httptest.NewRecorder()
	h.rotate(rec, dnsReq(tenant, http.MethodPost, "", pv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate=%d body=%s", rec.Code, rec.Body.String())
	}
	var key DKIMKey
	_ = json.Unmarshal(rec.Body.Bytes(), &key)

	// list → 200 with the key.
	rec = httptest.NewRecorder()
	h.list(rec, dnsReq(tenant, http.MethodGet, "", pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("list=%d", rec.Code)
	}

	// cross-tenant list → 403.
	rec = httptest.NewRecorder()
	h.list(rec, dnsReq("00000000-0000-0000-0000-000000000000", http.MethodGet, "", pv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-tenant list=%d want 403", rec.Code)
	}

	// PendingRotation surfaces the active key.
	pr, err := svc.PendingRotation(context.Background(), tenant, domainID, "")
	if err != nil {
		t.Fatalf("PendingRotation: %v", err)
	}
	if pr.NewKeyID != key.ID {
		t.Errorf("PendingRotation key=%s want %s", pr.NewKeyID, key.ID)
	}

	// LoadPrivateKey round-trips the stored PEM.
	pem, err := svc.LoadPrivateKey(context.Background(), tenant, domainID, key.ID)
	if err != nil || !strings.Contains(pem, "PRIVATE KEY") {
		t.Fatalf("LoadPrivateKey=%q err=%v", pem, err)
	}

	// revoke → 204.
	rec = httptest.NewRecorder()
	h.revoke(rec, dnsReq(tenant, http.MethodDelete, "", map[string]string{"id": tenant, "domainId": domainID, "keyId": key.ID}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("revoke=%d body=%s", rec.Code, rec.Body.String())
	}

	// PendingRotation with no active key + default record falls back.
	pr, err = svc.PendingRotation(context.Background(), tenant, domainID, "v=DKIM1; k=rsa; p=AAAA")
	if err != nil || pr.Selector != "kmail" {
		t.Errorf("PendingRotation default=%+v err=%v", pr, err)
	}
}

func TestDKIMHandlerErrorPathsDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewDKIMRotationService(pool, nil)
	domainID := seedDomain(t, svc, tenant)
	h := NewDKIMHandlers(svc, log.New(io.Discard, "", 0))

	// revoke a non-existent key → ErrNotFound → 404.
	rec := httptest.NewRecorder()
	h.revoke(rec, dnsReq(tenant, http.MethodDelete, "",
		map[string]string{"id": tenant, "domainId": domainID, "keyId": "00000000-0000-0000-0000-000000000000"}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("revoke missing key=%d want 404 body=%s", rec.Code, rec.Body.String())
	}

	// LoadPrivateKey validation: empty IDs → ErrInvalidInput.
	if _, err := svc.LoadPrivateKey(context.Background(), "", "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("LoadPrivateKey empty=%v want ErrInvalidInput", err)
	}
	// nil-pool LoadPrivateKey → pool error.
	if _, err := NewDKIMRotationService(nil, nil).LoadPrivateKey(context.Background(), "t", "d", "k"); err == nil {
		t.Error("LoadPrivateKey nil pool should error")
	}
}

func TestAutoconfigHandlers(t *testing.T) {
	svc := NewAutoconfigService(AutoconfigConfig{
		IMAPHost: "imap.kmail.test", IMAPPort: 993,
		SMTPHost: "smtp.kmail.test", SMTPPort: 587,
	})
	h := NewAutoconfigHandlers(svc, log.New(io.Discard, "", 0))
	mux := http.NewServeMux()
	h.Register(mux)

	// Mozilla autoconfig with a valid email → 200 XML.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mail/config-v1.1.xml?emailaddress=alice@example.com", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "imap.kmail.test") {
		t.Fatalf("mozilla autoconfig=%d body=%s", rec.Code, rec.Body.String())
	}

	// Missing email param → 400.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mail/config-v1.1.xml", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("mozilla no-email=%d want 400", rec.Code)
	}

	// Outlook autodiscover POST with a valid request body → 200 XML.
	reqXML := `<Autodiscover><Request><EMailAddress>bob@example.com</EMailAddress></Request></Autodiscover>`
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/autodiscover/autodiscover.xml", strings.NewReader(reqXML)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "smtp.kmail.test") {
		t.Fatalf("outlook autodiscover=%d body=%s", rec.Code, rec.Body.String())
	}

	// Invalid XML → 400.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/autodiscover/autodiscover.xml", strings.NewReader("<not-valid")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("outlook bad xml=%d want 400", rec.Code)
	}

	// Outlook with empty email → 400.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/autodiscover/autodiscover.xml",
		strings.NewReader(`<Autodiscover><Request><EMailAddress></EMailAddress></Request></Autodiscover>`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("outlook empty email=%d want 400", rec.Code)
	}
}
