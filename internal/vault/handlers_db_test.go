package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func req(ctx context.Context, method, body string, pv map[string]string) *http.Request {
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

func TestVaultHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	h := NewVaultHandlers(NewVaultService(pool), nil)
	ctx := context.Background()

	// create
	rec := httptest.NewRecorder()
	h.create(rec, req(ctx, http.MethodPost, `{"user_id":"u1","folder_name":"F","wrapped_dek":"YWJj","nonce":"bm9uY2U="}`, map[string]string{"id": tenant}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created Folder
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// list
	rec = httptest.NewRecorder()
	h.list(rec, req(ctx, http.MethodGet, "", map[string]string{"id": tenant}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}

	// get
	rec = httptest.NewRecorder()
	h.get(rec, req(ctx, http.MethodGet, "", map[string]string{"id": tenant, "folderId": created.ID}))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status=%d", rec.Code)
	}

	// setMeta
	rec = httptest.NewRecorder()
	h.setMeta(rec, req(ctx, http.MethodPut, `{"wrapped_dek":"YWJj","key_algorithm":"AES","nonce":"bm9uY2U="}`, map[string]string{"id": tenant, "folderId": created.ID}))
	if rec.Code != http.StatusOK {
		t.Fatalf("setMeta status=%d body=%s", rec.Code, rec.Body.String())
	}

	// create with bad JSON → 400
	rec = httptest.NewRecorder()
	h.create(rec, req(ctx, http.MethodPost, `{`, map[string]string{"id": tenant}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json status=%d want 400", rec.Code)
	}

	// delete
	rec = httptest.NewRecorder()
	h.delete(rec, req(ctx, http.MethodDelete, "", map[string]string{"id": tenant, "folderId": created.ID}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d", rec.Code)
	}
}

func TestProtectedHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	h := NewProtectedFolderHandlers(NewProtectedFolderService(pool), nil)
	ctx := context.Background()

	// create
	rec := httptest.NewRecorder()
	h.create(rec, req(ctx, http.MethodPost, `{"owner_id":"o1","folder_name":"Docs"}`, map[string]string{"id": tenant}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var f ProtectedFolder
	_ = json.Unmarshal(rec.Body.Bytes(), &f)

	// list
	rec = httptest.NewRecorder()
	h.list(rec, req(ctx, http.MethodGet, "", map[string]string{"id": tenant}))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d", rec.Code)
	}

	// share
	rec = httptest.NewRecorder()
	h.share(rec, req(ctx, http.MethodPost, `{"owner_id":"o1","grantee_id":"g1","permission":"read"}`, map[string]string{"id": tenant, "folderId": f.ID}))
	if rec.Code != http.StatusOK {
		t.Fatalf("share status=%d body=%s", rec.Code, rec.Body.String())
	}

	// access list
	rec = httptest.NewRecorder()
	h.access(rec, req(ctx, http.MethodGet, "", map[string]string{"id": tenant, "folderId": f.ID}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "g1") {
		t.Fatalf("access status=%d body=%s", rec.Code, rec.Body.String())
	}

	// unshare
	rec = httptest.NewRecorder()
	h.unshare(rec, req(ctx, http.MethodPost, `{"owner_id":"o1","grantee_id":"g1"}`, map[string]string{"id": tenant, "folderId": f.ID}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unshare status=%d", rec.Code)
	}

	// access-log
	rec = httptest.NewRecorder()
	h.accessLog(rec, req(ctx, http.MethodGet, "", map[string]string{"id": tenant, "folderId": f.ID}))
	if rec.Code != http.StatusOK {
		t.Fatalf("accessLog status=%d", rec.Code)
	}

	// share invalid permission → 400
	rec = httptest.NewRecorder()
	h.share(rec, req(ctx, http.MethodPost, `{"owner_id":"o1","grantee_id":"g2","permission":"root"}`, map[string]string{"id": tenant, "folderId": f.ID}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad permission status=%d want 400", rec.Code)
	}
}
