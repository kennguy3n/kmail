package vault

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestVaultHandlerErrorBranches drives the error paths by passing an
// empty tenant id, which the service rejects.
func TestVaultHandlerErrorBranches(t *testing.T) {
	h := NewVaultHandlers(NewVaultService(nil), nil)
	ctx := context.Background()
	empty := map[string]string{"id": "", "folderId": "f"}

	rec := httptest.NewRecorder()
	h.list(rec, req(ctx, http.MethodGet, "", empty))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list empty-tenant = %d want 500", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.get(rec, req(ctx, http.MethodGet, "", empty))
	if rec.Code != http.StatusNotFound {
		t.Errorf("get empty-tenant = %d want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.delete(rec, req(ctx, http.MethodDelete, "", empty))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("delete empty-tenant = %d want 500", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.setMeta(rec, req(ctx, http.MethodPut, `{"wrapped_dek":"YWJj","key_algorithm":"AES","nonce":"bm9u"}`, empty))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("setMeta empty-tenant = %d want 400", rec.Code)
	}

	// setMeta bad JSON → 400.
	rec = httptest.NewRecorder()
	h.setMeta(rec, req(ctx, http.MethodPut, `{bad`, map[string]string{"id": "t", "folderId": "f"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("setMeta bad json = %d want 400", rec.Code)
	}
}

// TestVaultRegisterRoutes exercises Register end-to-end via a mux
// behind dev-bypass OIDC.
func TestVaultRegisterRoutes(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	NewVaultHandlers(NewVaultService(pool), nil).Register(mux, authMW)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/"+tenant+"/vault/folders", nil)
	r.Header.Set("Authorization", "Bearer dev-token")
	r.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET folders via mux = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestProtectedHandlerErrorBranches + Register wiring.
func TestProtectedHandlerErrorBranches(t *testing.T) {
	h := NewProtectedFolderHandlers(NewProtectedFolderService(nil), nil)
	ctx := context.Background()
	empty := map[string]string{"id": "", "folderId": "f"}

	rec := httptest.NewRecorder()
	h.list(rec, req(ctx, http.MethodGet, "", empty))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("protected list empty-tenant = %d want 500", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.access(rec, req(ctx, http.MethodGet, "", empty))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("protected access empty-tenant = %d want 500", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.accessLog(rec, req(ctx, http.MethodGet, "", empty))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("protected accessLog empty-tenant = %d want 500", rec.Code)
	}

	// bad JSON branches.
	rec = httptest.NewRecorder()
	h.create(rec, req(ctx, http.MethodPost, `{bad`, map[string]string{"id": "t"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("protected create bad json = %d want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.unshare(rec, req(ctx, http.MethodPost, `{bad`, map[string]string{"id": "t", "folderId": "f"}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("protected unshare bad json = %d want 400", rec.Code)
	}
}

func TestProtectedRegisterRoutes(t *testing.T) {
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
	NewProtectedFolderHandlers(NewProtectedFolderService(pool), nil).Register(mux, authMW)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/"+tenant+"/protected-folders", nil)
	r.Header.Set("Authorization", "Bearer dev-token")
	r.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET protected-folders via mux = %d body=%s", rec.Code, rec.Body.String())
	}
}
