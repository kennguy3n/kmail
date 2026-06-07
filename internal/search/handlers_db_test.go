package search

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func searchReq(tenant, ctxTenant, method, body string) *http.Request {
	ctx := middleware.WithTenantID(context.Background(), ctxTenant)
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(ctx, method, "/x", nil)
	} else {
		r = httptest.NewRequestWithContext(ctx, method, "/x", strings.NewReader(body))
	}
	r.SetPathValue("id", tenant)
	return r
}

func dbSearchHandlers(t *testing.T) (*Handlers, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(Config{
		Pool:   pool,
		Logger: log.New(io.Discard, "", 0),
		Backends: []SearchBackend{
			&stubBackend{name: "shared_meilisearch"},
			&stubBackend{name: "shared_opensearch"},
		},
	})
	return NewHandlers(svc, log.New(io.Discard, "", 0)), tenant
}

func TestSearchHandlersBackendDB(t *testing.T) {
	h, tenant := dbSearchHandlers(t)

	// listBackends (global)
	rec := httptest.NewRecorder()
	h.listBackends(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "shared_meilisearch") {
		t.Fatalf("listBackends=%d body=%s", rec.Code, rec.Body.String())
	}

	// getBackend → defaults to shared_meilisearch
	rec = httptest.NewRecorder()
	h.getBackend(rec, searchReq(tenant, tenant, http.MethodGet, ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "shared_meilisearch") {
		t.Fatalf("getBackend=%d body=%s", rec.Code, rec.Body.String())
	}

	// putBackend → switch to shared_opensearch
	rec = httptest.NewRecorder()
	h.putBackend(rec, searchReq(tenant, tenant, http.MethodPut, `{"backend":"shared_opensearch"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("putBackend=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.getBackend(rec, searchReq(tenant, tenant, http.MethodGet, ""))
	if !strings.Contains(rec.Body.String(), "shared_opensearch") {
		t.Errorf("getBackend after put=%s", rec.Body.String())
	}

	// reindex → 202 queued
	rec = httptest.NewRecorder()
	h.reindex(rec, searchReq(tenant, tenant, http.MethodPost, ""))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reindex=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSearchHandlersScopeAndValidation(t *testing.T) {
	h, tenant := dbSearchHandlers(t)

	// Cross-tenant access → 403.
	rec := httptest.NewRecorder()
	h.getBackend(rec, searchReq(tenant, "00000000-0000-0000-0000-000000000000", http.MethodGet, ""))
	if rec.Code != http.StatusForbidden {
		t.Errorf("getBackend cross-tenant=%d want 403", rec.Code)
	}

	// Missing tenant context → 403.
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.SetPathValue("id", tenant)
	h.getBackend(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("getBackend no ctx=%d want 403", rec.Code)
	}

	// Unknown backend value → 400.
	rec = httptest.NewRecorder()
	h.putBackend(rec, searchReq(tenant, tenant, http.MethodPut, `{"backend":"not_a_backend"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("putBackend bad value=%d want 400 body=%s", rec.Code, rec.Body.String())
	}

	// Malformed JSON → 400.
	rec = httptest.NewRecorder()
	h.putBackend(rec, searchReq(tenant, tenant, http.MethodPut, `{bad`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("putBackend bad json=%d want 400", rec.Code)
	}
}
