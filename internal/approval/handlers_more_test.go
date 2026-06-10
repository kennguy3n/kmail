package approval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestApprovalRejectAndPendingList covers the reject handler and the
// status=pending list branch.
func TestApprovalRejectAndPendingList(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	h := NewHandlers(NewService(pool))
	ctx := context.Background()

	// create a request to reject.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/x",
		strings.NewReader(`{"requester_id":"r1","action":"user_delete","target_resource":"u-1"}`))
	req.SetPathValue("id", tenant)
	h.create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d body=%s", rec.Code, rec.Body.String())
	}
	var created Request
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// list?status=pending must include it.
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/x?status=pending", nil)
	req.SetPathValue("id", tenant)
	h.list(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending list=%d", rec.Code)
	}
	var pending []Request
	_ = json.Unmarshal(rec.Body.Bytes(), &pending)
	if len(pending) != 1 {
		t.Fatalf("pending list len=%d want 1", len(pending))
	}

	// reject.
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodPost, "/x",
		strings.NewReader(`{"approver_id":"a1","reason":"not allowed"}`))
	req.SetPathValue("id", tenant)
	req.SetPathValue("approvalId", created.ID)
	h.reject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestApprovalRejectError covers the reject error branch (unknown id).
func TestApprovalRejectError(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	h := NewHandlers(NewService(pool))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"approver_id":"a1"}`))
	req.SetPathValue("id", tenant)
	req.SetPathValue("approvalId", "00000000-0000-0000-0000-000000000000")
	h.reject(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("reject unknown=%d want 400", rec.Code)
	}

	// approve unknown id → 400.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"approver_id":"a1"}`))
	req.SetPathValue("id", tenant)
	req.SetPathValue("approvalId", "00000000-0000-0000-0000-000000000000")
	h.approve(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("approve unknown=%d want 400", rec.Code)
	}
}

// TestApprovalRegisterRoutes drives the registered routes through a mux
// behind dev-bypass OIDC so Register is exercised end-to-end.
func TestApprovalRegisterRoutes(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	NewHandlers(NewService(pool)).Register(mux, authMW)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/"+tenant+"/approvals", nil)
	r.Header.Set("Authorization", "Bearer dev-token")
	r.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET approvals via mux = %d body=%s", rec.Code, rec.Body.String())
	}

	// config route via mux.
	r = httptest.NewRequest(http.MethodGet, "/api/v1/tenants/"+tenant+"/approvals/config", nil)
	r.Header.Set("Authorization", "Bearer dev-token")
	r.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("GET config via mux = %d", rec.Code)
	}
}

// TestSetConfigBadJSON covers the setConfig decode-error branch.
func TestSetConfigBadJSON(t *testing.T) {
	h := NewHandlers(NewService(nil))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader("{bad"))
	req.SetPathValue("id", "t")
	h.setConfig(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("setConfig bad json=%d want 400", rec.Code)
	}
}
