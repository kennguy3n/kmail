package retention

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func retReq(tenant, method, body string, pv map[string]string) *http.Request {
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

func TestRetentionHandlersCRUDDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(pool)
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	idv := map[string]string{"id": tenant}

	// list (empty) → 200 with [] not null.
	rec := httptest.NewRecorder()
	h.list(rec, retReq(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) == "null" {
		t.Fatalf("list empty=%d body=%s", rec.Code, rec.Body.String())
	}

	// create → 201.
	rec = httptest.NewRecorder()
	h.create(rec, retReq(tenant, http.MethodPost,
		`{"policy_type":"delete","retention_days":30,"applies_to":"all","enabled":true}`, idv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d body=%s", rec.Code, rec.Body.String())
	}
	var p Policy
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	t.Cleanup(func() { _ = svc.DeletePolicy(context.Background(), tenant, p.ID) })

	// update → 200.
	rec = httptest.NewRecorder()
	h.update(rec, retReq(tenant, http.MethodPut,
		`{"policy_type":"delete","retention_days":60,"applies_to":"all","enabled":true}`,
		map[string]string{"id": tenant, "policyId": p.ID}))
	if rec.Code != http.StatusOK {
		t.Fatalf("update=%d body=%s", rec.Code, rec.Body.String())
	}

	// status without a worker → defaults dry_run=true.
	rec = httptest.NewRecorder()
	h.status(rec, retReq(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var st map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st["dry_run"] != true {
		t.Errorf("status dry_run=%v want true (no worker wired)", st["dry_run"])
	}

	// status WITH a worker reflects the worker snapshot.
	w := NewWorker(svc, log.New(io.Discard, "", 0)).WithDryRun(false)
	rec = httptest.NewRecorder()
	NewHandlers(svc, nil).WithWorker(w).status(rec, retReq(tenant, http.MethodGet, "", idv))
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st["dry_run"] != false {
		t.Errorf("status with worker dry_run=%v want false", st["dry_run"])
	}

	// delete → 204.
	rec = httptest.NewRecorder()
	h.delete(rec, retReq(tenant, http.MethodDelete, "", map[string]string{"id": tenant, "policyId": p.ID}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("delete=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRetentionHandlerBadInputDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	h := NewHandlers(NewService(pool), log.New(io.Discard, "", 0))
	idv := map[string]string{"id": tenant}

	// malformed JSON → 400.
	for _, fn := range []http.HandlerFunc{h.create, h.update} {
		rec := httptest.NewRecorder()
		fn(rec, retReq(tenant, http.MethodPost, `{bad`, idv))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad json=%d want 400", rec.Code)
		}
	}

	// valid JSON but invalid policy (bad type) → 400 from CreatePolicy.
	rec := httptest.NewRecorder()
	h.create(rec, retReq(tenant, http.MethodPost,
		`{"policy_type":"nope","retention_days":5,"applies_to":"all"}`, idv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid policy=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
}

func TestRetentionRegisterMountsRoutesDB(t *testing.T) {
	pool := testsupport.Pool(t)
	h := NewHandlers(NewService(pool), log.New(io.Discard, "", 0))
	authMW := middleware.MustNewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-secret",
		Env:            middleware.EnvDevelopment,
	})
	mux := http.NewServeMux()
	h.Register(mux, authMW)
	for _, p := range []string{
		"/api/v1/tenants/t1/retention",
		"/api/v1/tenants/t1/retention/status",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("route %s not mounted (404)", p)
		}
	}
}
