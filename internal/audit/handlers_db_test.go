package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// passthroughAuth is a no-op Registrar so Register can be exercised
// without standing up the full OIDC middleware.
type passthroughAuth struct{}

func (passthroughAuth) Wrap(h http.Handler) http.Handler { return h }

// auditReq builds a request with the tenantID path value populated,
// matching how the std-lib mux injects it in production.
func auditReq(method, target, tenant string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	if tenant != "" {
		r.SetPathValue("tenantID", tenant)
	}
	return r
}

func TestAuditHandlersQueryExportVerify(t *testing.T) {
	svc, tenant, admin := dbService(t)
	ctx := context.Background()
	h := NewHandlers(svc, nil) // nil logger → log.Default()

	e1, err := svc.Log(ctx, Entry{
		TenantID: tenant, ActorID: "admin-1", ActorType: ActorAdmin,
		Action: "user.create", ResourceType: "user", ResourceID: "u-1",
	})
	if err != nil {
		t.Fatalf("Log e1: %v", err)
	}
	e2, err := svc.Log(ctx, Entry{
		TenantID: tenant, ActorID: "admin-1", ActorType: ActorAdmin,
		Action: "user.delete", ResourceType: "user", ResourceID: "u-1",
	})
	if err != nil {
		t.Fatalf("Log e2: %v", err)
	}

	// query: filters + pagination + time bounds all parsed.
	rec := httptest.NewRecorder()
	r := auditReq(http.MethodGet, "/x?action=user.delete&actor=admin-1&resource_type=user&limit=10&offset=0&since=2000-01-01T00:00:00Z&until=2999-01-01T00:00:00Z", tenant)
	h.query(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("query=%d body=%s", rec.Code, rec.Body.String())
	}
	var qr struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &qr); err != nil {
		t.Fatalf("query decode: %v", err)
	}
	if len(qr.Entries) != 1 {
		t.Errorf("query filtered entries=%d want 1", len(qr.Entries))
	}

	// query with empty tenant ⇒ ErrInvalidInput ⇒ 400 (respondError).
	rec = httptest.NewRecorder()
	h.query(rec, auditReq(http.MethodGet, "/x", ""))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("query empty tenant=%d want 400", rec.Code)
	}

	// export json (default) + csv.
	rec = httptest.NewRecorder()
	h.export(rec, auditReq(http.MethodGet, "/x", tenant))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("export json=%d ct=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec = httptest.NewRecorder()
	h.export(rec, auditReq(http.MethodGet, "/x?format=csv&since=2000-01-01T00:00:00Z&until=2999-01-01T00:00:00Z", tenant))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/csv") {
		t.Errorf("export csv=%d ct=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	// export invalid format ⇒ ErrInvalidInput ⇒ 400.
	rec = httptest.NewRecorder()
	h.export(rec, auditReq(http.MethodGet, "/x?format=xml", tenant))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("export xml=%d want 400", rec.Code)
	}

	// verify intact chain ⇒ 200.
	rec = httptest.NewRecorder()
	h.verify(rec, auditReq(http.MethodPost, "/x", tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("verify intact=%d body=%s", rec.Code, rec.Body.String())
	}

	// Tamper → verify returns 409 Conflict (ErrChainBroken).
	if _, err := admin.Exec(ctx, `UPDATE audit_log SET action='tampered' WHERE id=$1::uuid`, e2.ID); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	rec = httptest.NewRecorder()
	h.verify(rec, auditReq(http.MethodPost, "/x", tenant))
	if rec.Code != http.StatusConflict {
		t.Errorf("verify tampered=%d want 409", rec.Code)
	}
	_ = e1
}

// TestAuditHandlersRegister verifies the routes are mounted and flow
// through the auth wrapper to the handlers.
func TestAuditHandlersRegister(t *testing.T) {
	svc, tenant, _ := dbService(t)
	h := NewHandlers(svc, nil)
	mux := http.NewServeMux()
	h.Register(mux, passthroughAuth{})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tenants/"+tenant+"/audit-log", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("routed query=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuditHandlerTenantIDFallback covers the context-based tenant
// resolution path when no path value is present.
func TestAuditHandlerTenantIDFallback(t *testing.T) {
	svc, tenant, _ := dbService(t)
	h := NewHandlers(svc, nil)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r = r.WithContext(middleware.WithTenantID(context.Background(), tenant))
	h.query(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("query via context tenant=%d body=%s", rec.Code, rec.Body.String())
	}
}
