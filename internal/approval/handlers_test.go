package approval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	h := NewHandlers(NewService(pool))
	ctx := context.Background()

	// create
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/tenants/"+tenant+"/approvals",
		strings.NewReader(`{"requester_id":"r1","action":"user_delete","target_resource":"u-1"}`))
	req.SetPathValue("id", tenant)
	h.create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created Request
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// list (default = all)
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/x", nil)
	req.SetPathValue("id", tenant)
	h.list(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d", rec.Code)
	}
	var listed []Request
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed) != 1 {
		t.Fatalf("list body=%s err=%v", rec.Body.String(), err)
	}

	// approve
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodPost, "/x",
		strings.NewReader(`{"approver_id":"a1"}`))
	req.SetPathValue("id", tenant)
	req.SetPathValue("approvalId", created.ID)
	h.approve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", rec.Code, rec.Body.String())
	}

	// execute → no executor registered → 501
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodPost, "/x", nil)
	req.SetPathValue("id", tenant)
	req.SetPathValue("approvalId", created.ID)
	h.execute(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("execute status=%d want 501", rec.Code)
	}

	// setConfig + config
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodPut, "/x",
		strings.NewReader(`{"user_delete":true}`))
	req.SetPathValue("id", tenant)
	h.setConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setConfig status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/x", nil)
	req.SetPathValue("id", tenant)
	h.config(rec, req)
	var cfg map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil || !cfg["user_delete"] {
		t.Fatalf("config body=%s err=%v", rec.Body.String(), err)
	}
}

func TestHandlersBadJSON(t *testing.T) {
	h := NewHandlers(NewService(nil))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{bad"))
	req.SetPathValue("id", "t")
	h.create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json create status=%d want 400", rec.Code)
	}
}
