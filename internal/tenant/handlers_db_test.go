package tenant

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

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// scopedReq builds a request whose context carries the tenant id so
// checkTenantScope passes, and sets the path values the handler reads.
func scopedReq(tenant, method string, body string, pathVals map[string]string) *http.Request {
	ctx := middleware.WithTenantID(context.Background(), tenant)
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(ctx, method, "/x", nil)
	} else {
		r = httptest.NewRequestWithContext(ctx, method, "/x", strings.NewReader(body))
	}
	for k, v := range pathVals {
		r.SetPathValue(k, v)
	}
	return r
}

func TestTenantHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	h := NewHandlers(svc, testLogger())
	ctx := context.Background()
	u := uniq()

	// Create a tenant via the service (createTenant handler exercised separately).
	tn, err := svc.CreateTenant(ctx, CreateTenantInput{Name: "H " + u, Slug: "h-" + u, Plan: "pro"})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	idVals := map[string]string{"id": tn.ID}

	// getTenant
	rec := httptest.NewRecorder()
	h.getTenant(rec, scopedReq(tn.ID, http.MethodGet, "", idVals))
	if rec.Code != http.StatusOK {
		t.Fatalf("getTenant=%d body=%s", rec.Code, rec.Body.String())
	}

	// listTenants
	rec = httptest.NewRecorder()
	h.listTenants(rec, scopedReq(tn.ID, http.MethodGet, "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("listTenants=%d", rec.Code)
	}

	// updateTenant
	rec = httptest.NewRecorder()
	h.updateTenant(rec, scopedReq(tn.ID, http.MethodPatch, `{"status":"suspended"}`, idVals))
	if rec.Code != http.StatusOK {
		t.Fatalf("updateTenant=%d body=%s", rec.Code, rec.Body.String())
	}

	// Cross-tenant scope is rejected with 403.
	rec = httptest.NewRecorder()
	bad := scopedReq("00000000-0000-0000-0000-000000000000", http.MethodGet, "", idVals)
	h.listUsers(rec, bad)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-tenant listUsers=%d want 403", rec.Code)
	}

	// createUser
	rec = httptest.NewRecorder()
	h.createUser(rec, scopedReq(tn.ID, http.MethodPost,
		`{"kchat_user_id":"kc-`+u+`","stalwart_account_id":"sw-`+u+`","email":"u-`+u+`@example.com","display_name":"U","role":"member"}`, idVals))
	if rec.Code != http.StatusCreated {
		t.Fatalf("createUser=%d body=%s", rec.Code, rec.Body.String())
	}
	var usr User
	if err := json.Unmarshal(rec.Body.Bytes(), &usr); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	userVals := map[string]string{"id": tn.ID, "userId": usr.ID}

	// listUsers / getUser / updateUser
	rec = httptest.NewRecorder()
	h.listUsers(rec, scopedReq(tn.ID, http.MethodGet, "", idVals))
	if rec.Code != http.StatusOK {
		t.Errorf("listUsers=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.getUser(rec, scopedReq(tn.ID, http.MethodGet, "", userVals))
	if rec.Code != http.StatusOK {
		t.Errorf("getUser=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.updateUser(rec, scopedReq(tn.ID, http.MethodPatch, `{"display_name":"Renamed"}`, userVals))
	if rec.Code != http.StatusOK {
		t.Errorf("updateUser=%d body=%s", rec.Code, rec.Body.String())
	}

	// createDomain / listDomains
	rec = httptest.NewRecorder()
	h.createDomain(rec, scopedReq(tn.ID, http.MethodPost, `{"domain":"d-`+u+`.example.com"}`, idVals))
	if rec.Code != http.StatusCreated {
		t.Fatalf("createDomain=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.listDomains(rec, scopedReq(tn.ID, http.MethodGet, "", idVals))
	if rec.Code != http.StatusOK {
		t.Errorf("listDomains=%d", rec.Code)
	}

	// createAlias / listAliases / listUserAliases / deleteAlias
	rec = httptest.NewRecorder()
	h.createAlias(rec, scopedReq(tn.ID, http.MethodPost,
		`{"user_id":"`+usr.ID+`","alias_email":"alias-`+u+`@example.com"}`, idVals))
	if rec.Code != http.StatusCreated {
		t.Fatalf("createAlias=%d body=%s", rec.Code, rec.Body.String())
	}
	var al Alias
	_ = json.Unmarshal(rec.Body.Bytes(), &al)
	rec = httptest.NewRecorder()
	h.listAliases(rec, scopedReq(tn.ID, http.MethodGet, "", idVals))
	if rec.Code != http.StatusOK {
		t.Errorf("listAliases=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.listUserAliases(rec, scopedReq(tn.ID, http.MethodGet, "", userVals))
	if rec.Code != http.StatusOK {
		t.Errorf("listUserAliases=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.deleteAlias(rec, scopedReq(tn.ID, http.MethodDelete, "", map[string]string{"id": tn.ID, "aliasId": al.ID}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("deleteAlias=%d body=%s", rec.Code, rec.Body.String())
	}

	// shared inbox + members
	rec = httptest.NewRecorder()
	h.createSharedInbox(rec, scopedReq(tn.ID, http.MethodPost,
		`{"address":"team-`+u+`@example.com","display_name":"Team","mls_group_id":"mls-`+u+`"}`, idVals))
	if rec.Code != http.StatusCreated {
		t.Fatalf("createSharedInbox=%d body=%s", rec.Code, rec.Body.String())
	}
	var si SharedInbox
	_ = json.Unmarshal(rec.Body.Bytes(), &si)
	rec = httptest.NewRecorder()
	h.listSharedInboxes(rec, scopedReq(tn.ID, http.MethodGet, "", idVals))
	if rec.Code != http.StatusOK {
		t.Errorf("listSharedInboxes=%d", rec.Code)
	}
	inboxVals := map[string]string{"id": tn.ID, "inboxId": si.ID}
	rec = httptest.NewRecorder()
	h.addSharedInboxMember(rec, scopedReq(tn.ID, http.MethodPost, `{"user_id":"`+usr.ID+`","role":"member"}`, inboxVals))
	if rec.Code != http.StatusCreated {
		t.Fatalf("addSharedInboxMember=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.removeSharedInboxMember(rec, scopedReq(tn.ID, http.MethodDelete, "",
		map[string]string{"id": tn.ID, "inboxId": si.ID, "userId": usr.ID}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("removeSharedInboxMember=%d body=%s", rec.Code, rec.Body.String())
	}

	// deleteUser
	rec = httptest.NewRecorder()
	h.deleteUser(rec, scopedReq(tn.ID, http.MethodDelete, "", userVals))
	if rec.Code != http.StatusNoContent {
		t.Errorf("deleteUser=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTenantHandlersBadRequest(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	h := NewHandlers(svc, testLogger())
	tn := "11111111-1111-1111-1111-111111111111"

	// Malformed JSON → 400.
	rec := httptest.NewRecorder()
	h.createUser(rec, scopedReq(tn, http.MethodPost, `{bad`, map[string]string{"id": tn}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad createUser=%d want 400", rec.Code)
	}
}
