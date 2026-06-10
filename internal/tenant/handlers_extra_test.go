package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestStatusForServiceError maps each sentinel to its HTTP status —
// pure, no database required.
func TestStatusForServiceError(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: bad", ErrInvalidInput), http.StatusBadRequest},
		{fmt.Errorf("wrap: %w", ErrNotFound), http.StatusNotFound},
		{ErrAliasInUse, http.StatusConflict},
		{errors.New("postgres exploded"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := statusForServiceError(c.err); got != c.want {
			t.Errorf("statusForServiceError(%v)=%d want %d", c.err, got, c.want)
		}
	}
}

// TestHandlersRegister installs every route on a mux through a
// dev-mode OIDC wrapper and asserts a representative route resolves
// (i.e. Register wired it).
func TestHandlersRegister(t *testing.T) {
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{Env: middleware.EnvDevelopment})
	if err != nil {
		t.Fatalf("NewOIDC dev: %v", err)
	}
	h := NewHandlers(NewService(nil), testLogger())
	mux := http.NewServeMux()
	h.Register(mux, authMW)

	// A registered pattern resolves to a non-nil handler; an
	// unregistered method/path falls back to the mux 404 handler.
	if hdlr, pat := mux.Handler(httptest.NewRequest(http.MethodPost, "/api/v1/tenants", nil)); pat == "" || hdlr == nil {
		t.Errorf("POST /api/v1/tenants not registered (pattern=%q)", pat)
	}
	if _, pat := mux.Handler(httptest.NewRequest(http.MethodGet, "/api/v1/tenants/abc/users/xyz/aliases", nil)); pat == "" {
		t.Error("nested aliases route not registered")
	}
}

// TestCreateDeleteTenantHandlersDB covers the createTenant and
// deleteTenant handlers end to end, plus their validation/not-found
// error branches.
func TestCreateDeleteTenantHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	h := NewHandlers(svc, testLogger())
	u := uniq()

	// Happy path: 201 with a fresh tenant.
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"name":"Reg %s","slug":"reg-%s","plan":"pro"}`, u, u)
	h.createTenant(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("createTenant=%d body=%s", rec.Code, rec.Body.String())
	}
	var created Tenant
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, created.ID)
	})

	// Malformed JSON → 400.
	rec = httptest.NewRecorder()
	h.createTenant(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{nope`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createTenant bad json=%d want 400", rec.Code)
	}

	// Missing required fields → service ErrInvalidInput → 400.
	rec = httptest.NewRecorder()
	h.createTenant(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("createTenant missing fields=%d want 400", rec.Code)
	}

	// deleteTenant happy path → 204.
	rec = httptest.NewRecorder()
	h.deleteTenant(rec, scopedReq(created.ID, http.MethodDelete, "", map[string]string{"id": created.ID}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("deleteTenant=%d body=%s", rec.Code, rec.Body.String())
	}

	// deleteTenant on a random UUID → ErrNotFound → 404.
	rec = httptest.NewRecorder()
	h.deleteTenant(rec, scopedReq("x", http.MethodDelete, "", map[string]string{"id": "00000000-0000-0000-0000-000000000000"}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleteTenant missing=%d want 404", rec.Code)
	}
}

// TestHandlersCrossTenantForbidden asserts every tenant-scoped
// handler rejects a request whose context tenant differs from the
// path tenant with 403 — the core isolation guard.
func TestHandlersCrossTenantForbidden(t *testing.T) {
	h := NewHandlers(NewService(nil), testLogger())
	const pathTenant = "11111111-1111-1111-1111-111111111111"
	other := "22222222-2222-2222-2222-222222222222"
	idVals := map[string]string{"id": pathTenant}
	userVals := map[string]string{"id": pathTenant, "userId": "u1"}
	inboxVals := map[string]string{"id": pathTenant, "inboxId": "i1", "userId": "u1"}

	type tc struct {
		name string
		fn   http.HandlerFunc
		req  *http.Request
	}
	cases := []tc{
		{"createUser", h.createUser, scopedReq(other, http.MethodPost, `{}`, idVals)},
		{"listUsers", h.listUsers, scopedReq(other, http.MethodGet, "", idVals)},
		{"getUser", h.getUser, scopedReq(other, http.MethodGet, "", userVals)},
		{"updateUser", h.updateUser, scopedReq(other, http.MethodPatch, `{}`, userVals)},
		{"deleteUser", h.deleteUser, scopedReq(other, http.MethodDelete, "", userVals)},
		{"createDomain", h.createDomain, scopedReq(other, http.MethodPost, `{}`, idVals)},
		{"listDomains", h.listDomains, scopedReq(other, http.MethodGet, "", idVals)},
		{"createAlias", h.createAlias, scopedReq(other, http.MethodPost, `{}`, idVals)},
		{"listAliases", h.listAliases, scopedReq(other, http.MethodGet, "", idVals)},
		{"listUserAliases", h.listUserAliases, scopedReq(other, http.MethodGet, "", userVals)},
		{"deleteAlias", h.deleteAlias, scopedReq(other, http.MethodDelete, "", map[string]string{"id": pathTenant, "aliasId": "a1"})},
		{"createSharedInbox", h.createSharedInbox, scopedReq(other, http.MethodPost, `{}`, idVals)},
		{"listSharedInboxes", h.listSharedInboxes, scopedReq(other, http.MethodGet, "", idVals)},
		{"addSharedInboxMember", h.addSharedInboxMember, scopedReq(other, http.MethodPost, `{}`, inboxVals)},
		{"removeSharedInboxMember", h.removeSharedInboxMember, scopedReq(other, http.MethodDelete, "", inboxVals)},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.fn(rec, c.req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s cross-tenant=%d want 403", c.name, rec.Code)
		}
	}
}

// TestHandlersBadJSON asserts the decode handlers reject malformed
// bodies with 400 (the in-scope tenant passes checkTenantScope so we
// reach the decode step).
func TestHandlersBadJSON(t *testing.T) {
	h := NewHandlers(NewService(nil), testLogger())
	const tn = "11111111-1111-1111-1111-111111111111"
	idVals := map[string]string{"id": tn}
	userVals := map[string]string{"id": tn, "userId": "u1"}
	inboxVals := map[string]string{"id": tn, "inboxId": "i1"}

	cases := []struct {
		name string
		fn   http.HandlerFunc
		req  *http.Request
	}{
		{"updateTenant", h.updateTenant, scopedReq(tn, http.MethodPatch, `{bad`, idVals)},
		{"createUser", h.createUser, scopedReq(tn, http.MethodPost, `{bad`, idVals)},
		{"updateUser", h.updateUser, scopedReq(tn, http.MethodPatch, `{bad`, userVals)},
		{"createDomain", h.createDomain, scopedReq(tn, http.MethodPost, `{bad`, idVals)},
		{"createAlias", h.createAlias, scopedReq(tn, http.MethodPost, `{bad`, idVals)},
		{"createSharedInbox", h.createSharedInbox, scopedReq(tn, http.MethodPost, `{bad`, idVals)},
		{"addSharedInboxMember", h.addSharedInboxMember, scopedReq(tn, http.MethodPost, `{bad`, inboxVals)},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.fn(rec, c.req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s bad json=%d want 400", c.name, rec.Code)
		}
	}
}
