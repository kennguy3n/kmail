package integrations

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/oauth"
	"github.com/kennguy3n/kmail/internal/webhooks"
)

// dbHandlers returns Handlers wired to the DB-backed Service in the
// harness, plus a request builder that injects an OAuth2 access
// token context (bypassing the bearer middleware, which is covered
// separately by the Register wiring test).
func dbHandlers(t *testing.T, h *dbHarness) (*Handlers, func(method, target string, body any, webhookID string) *httptest.ResponseRecorder) {
	t.Helper()
	hd := NewHandlers(h.svc, log.New(io.Discard, "", 0))
	call := func(method, target string, body any, webhookID string) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, target, rdr)
		ctx := oauth.WithAccessTokenContext(req.Context(), &oauth.AccessTokenContext{
			TenantID: h.tenant,
			ClientID: h.clientID,
			UserID:   h.userID,
			Scopes:   []string{oauth.ScopeReadMail, oauth.ScopeReadCalendar},
		})
		req = req.WithContext(ctx)
		if webhookID != "" {
			req.SetPathValue("webhookId", webhookID)
		}
		rec := httptest.NewRecorder()
		switch {
		case method == http.MethodPost && webhookID != "":
			hd.testFire(rec, req)
		case method == http.MethodPost:
			hd.register(rec, req)
		case method == http.MethodGet:
			hd.list(rec, req)
		case method == http.MethodDelete:
			hd.del(rec, req)
		}
		return rec
	}
	return hd, call
}

// TestHandlersRegisterListDeleteHTTP drives the HTTP handler bodies
// over the live Service.
func TestHandlersRegisterListDeleteHTTP(t *testing.T) {
	h := newDBHarness(t, nil)
	_, call := dbHandlers(t, h)

	// register → 201.
	rec := call(http.MethodPost, "/api/v1/integ/webhooks", registerRequest{
		URL:    "https://hook.example.com/http",
		Events: []string{webhooks.EventEmailReceived},
	}, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var reg registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reg.Endpoint == nil || reg.Secret == "" {
		t.Fatalf("missing endpoint/secret: %+v", reg)
	}

	// list → 200 with one row.
	rec = call(http.MethodGet, "/api/v1/integ/webhooks", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code=%d", rec.Code)
	}
	var eps []webhooks.Endpoint
	_ = json.Unmarshal(rec.Body.Bytes(), &eps)
	if len(eps) != 1 {
		t.Fatalf("list returned %d", len(eps))
	}

	// test-fire → 202.
	rec = call(http.MethodPost, "/api/v1/integ/webhooks/x/test", nil, reg.Endpoint.ID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test-fire: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// delete → 204.
	rec = call(http.MethodDelete, "/api/v1/integ/webhooks/x", nil, reg.Endpoint.ID)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// delete again → 404.
	rec = call(http.MethodDelete, "/api/v1/integ/webhooks/x", nil, reg.Endpoint.ID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("repeat delete: code=%d want 404", rec.Code)
	}

	// test-fire unknown → 404.
	rec = call(http.MethodPost, "/api/v1/integ/webhooks/x/test", nil, "00000000-0000-0000-0000-000000000000")
	if rec.Code != http.StatusNotFound {
		t.Errorf("test-fire unknown: code=%d want 404", rec.Code)
	}
}

// TestHandlersRegisterErrorsHTTP covers the register handler's
// boundary validation + insufficient-scope (422) branches.
func TestHandlersRegisterErrorsHTTP(t *testing.T) {
	h := newDBHarness(t, nil)
	hd, call := dbHandlers(t, h)

	// Bad JSON → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/integ/webhooks", bytes.NewReader([]byte("{bad")))
	req = req.WithContext(oauth.WithAccessTokenContext(req.Context(), &oauth.AccessTokenContext{
		TenantID: h.tenant, ClientID: h.clientID, UserID: h.userID, Scopes: []string{oauth.ScopeReadMail},
	}))
	rec := httptest.NewRecorder()
	hd.register(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: code=%d want 400", rec.Code)
	}

	// Missing URL → 400.
	rec = call(http.MethodPost, "/api/v1/integ/webhooks", registerRequest{Events: []string{webhooks.EventEmailReceived}}, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing url: code=%d want 400", rec.Code)
	}

	// Empty events → 400.
	rec = call(http.MethodPost, "/api/v1/integ/webhooks", registerRequest{URL: "https://x"}, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty events: code=%d want 400", rec.Code)
	}

	// Calendar event with only read:mail scope → all denied → 422.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/integ/webhooks", mustJSON(registerRequest{
		URL: "https://x", Events: []string{webhooks.EventCalendarCreated},
	}))
	req = req.WithContext(oauth.WithAccessTokenContext(req.Context(), &oauth.AccessTokenContext{
		TenantID: h.tenant, ClientID: h.clientID, UserID: h.userID, Scopes: []string{oauth.ScopeReadMail},
	}))
	rec = httptest.NewRecorder()
	hd.register(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("all denied: code=%d want 422", rec.Code)
	}
}

// TestHandlersMissingTokenContext pins the 401 guards on every
// handler when no token context is present.
func TestHandlersMissingTokenContext(t *testing.T) {
	hd := NewHandlers(&Service{cfg: ServiceConfig{}}, log.New(io.Discard, "", 0))
	for _, fn := range []http.HandlerFunc{hd.register, hd.list, hd.del, hd.testFire} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integ/webhooks", nil)
		rec := httptest.NewRecorder()
		fn(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("missing token ctx: code=%d want 401", rec.Code)
		}
	}
}

// TestRegisterRouteWiring verifies NewHandlers + Register install
// the routes behind the OAuth2 bearer middleware (no token → 401).
func TestRegisterRouteWiring(t *testing.T) {
	h := newDBHarness(t, nil)
	authMW := oauth.NewAuthMiddleware(h.svc.cfg.OAuth)
	mux := http.NewServeMux()
	NewHandlers(h.svc, nil).Register(mux, authMW)

	for _, route := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/integ/webhooks"},
		{http.MethodGet, "/api/v1/integ/webhooks"},
		{http.MethodDelete, "/api/v1/integ/webhooks/abc"},
		{http.MethodPost, "/api/v1/integ/webhooks/abc/test"},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: code=%d want 401 (no bearer)", route.method, route.path, rec.Code)
		}
	}
}

func mustJSON(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}
