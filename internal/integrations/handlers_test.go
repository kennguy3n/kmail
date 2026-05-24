package integrations

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/oauth"
)

// TestRequireAnyIntegrationScope_AllowsAnyEventEligibleScope
// pins the boundary middleware: a token carrying ANY scope
// that maps to an integration event passes through.
func TestRequireAnyIntegrationScope_AllowsAnyEventEligibleScope(t *testing.T) {
	cases := []struct {
		name   string
		scopes []string
	}{
		{"read:mail alone", []string{oauth.ScopeReadMail}},
		{"read:calendar alone", []string{oauth.ScopeReadCalendar}},
		{"write:mail (implies read:mail)", []string{oauth.ScopeWriteMail}},
		{"write:calendar (implies read:calendar)", []string{oauth.ScopeWriteCalendar}},
		{"multiple scopes", []string{oauth.ScopeReadMail, oauth.ScopeReadCalendar}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandlersForTest(t)
			downstream := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downstream = true
				w.WriteHeader(http.StatusOK)
			})
			mw := h.requireAnyIntegrationScope(next)

			req := httptest.NewRequest("GET", "/api/v1/integ/webhooks", nil)
			req = req.WithContext(withTokenCtx(req.Context(), &oauth.AccessTokenContext{
				TenantID: "tenant-1",
				ClientID: "client-1",
				Scopes:   tc.scopes,
			}))
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)

			if !downstream {
				t.Errorf("downstream handler not reached; got status=%d body=%s", rec.Code, rec.Body.String())
			}
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d; want %d", rec.Code, http.StatusOK)
			}
		})
	}
}

// TestRequireAnyIntegrationScope_RejectsNoIntegrationScopes
// pins the boundary deny path: a token without any integration-
// eligible scope receives 403 with insufficient_scope.
func TestRequireAnyIntegrationScope_RejectsNoIntegrationScopes(t *testing.T) {
	h := newHandlersForTest(t)
	downstreamCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamCalled = true
	})
	mw := h.requireAnyIntegrationScope(next)

	// read:profile is NOT in EventRequiredScope (no event maps
	// to it) so it MUST NOT pass the boundary check. A typo or
	// future scope addition that introduces a read:profile
	// event would force this test to update.
	req := httptest.NewRequest("GET", "/api/v1/integ/webhooks", nil)
	req = req.WithContext(withTokenCtx(req.Context(), &oauth.AccessTokenContext{
		TenantID: "tenant-1",
		ClientID: "client-1",
		Scopes:   []string{oauth.ScopeReadProfile},
	}))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if downstreamCalled {
		t.Error("downstream reached despite no integration-eligible scope")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusForbidden)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("non-JSON response body: %s", rec.Body.String())
	}
	if body["error"] != "insufficient_scope" {
		t.Errorf("error code = %q; want %q", body["error"], "insufficient_scope")
	}
}

// TestRequireAnyIntegrationScope_RejectsMissingTokenContext
// pins the defensive contract: if AuthMiddleware did not set
// the token context (e.g. due to a refactor that broke the
// chain), the integration handler MUST refuse rather than
// fall through to an anonymous call.
func TestRequireAnyIntegrationScope_RejectsMissingTokenContext(t *testing.T) {
	h := newHandlersForTest(t)
	mw := h.requireAnyIntegrationScope(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("downstream reached without token context")
	}))
	req := httptest.NewRequest("GET", "/api/v1/integ/webhooks", nil)
	// no token context attached
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestRegister_RejectsMalformedBody pins the input-validation
// short-circuit: a malformed JSON body MUST be answered with
// 400 invalid_request and MUST NOT touch the database.
func TestRegister_RejectsMalformedBody(t *testing.T) {
	h := newHandlersForTest(t)
	req := httptest.NewRequest("POST", "/api/v1/integ/webhooks", strings.NewReader("{not-json"))
	req = req.WithContext(withTokenCtx(req.Context(), &oauth.AccessTokenContext{
		TenantID: "tenant-1", ClientID: "client-1", Scopes: []string{oauth.ScopeReadMail},
	}))
	rec := httptest.NewRecorder()
	h.register(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusBadRequest)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_request" {
		t.Errorf("error code = %q; want invalid_request", body["error"])
	}
}

// TestRegister_RejectsEmptyURL pins the URL-required validation.
func TestRegister_RejectsEmptyURL(t *testing.T) {
	h := newHandlersForTest(t)
	bodyJSON := `{"url": "", "events": ["email.received"]}`
	req := httptest.NewRequest("POST", "/api/v1/integ/webhooks", strings.NewReader(bodyJSON))
	req = req.WithContext(withTokenCtx(req.Context(), &oauth.AccessTokenContext{
		TenantID: "tenant-1", ClientID: "client-1", Scopes: []string{oauth.ScopeReadMail},
	}))
	rec := httptest.NewRecorder()
	h.register(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestRegister_RejectsEmptyEvents pins the events-required
// validation. A registration with an empty events array would
// previously bypass the FilterEventsForClient insufficient-
// scope guard and store a wildcard subscription (`events = []`)
// that the underlying webhooks package interprets as "deliver
// every event". Defense-in-depth: dispatch-time
// EventAllowedForClient still gates on the client's actual
// scopes, so no privilege escalation, but the row is broader
// than the client's intent — reject at the boundary so the
// integration MUST enumerate its subscriptions.
func TestRegister_RejectsEmptyEvents(t *testing.T) {
	h := newHandlersForTest(t)
	bodyJSON := `{"url": "https://example.com/hook", "events": []}`
	req := httptest.NewRequest("POST", "/api/v1/integ/webhooks", strings.NewReader(bodyJSON))
	req = req.WithContext(withTokenCtx(req.Context(), &oauth.AccessTokenContext{
		TenantID: "tenant-1", ClientID: "client-1", Scopes: []string{oauth.ScopeReadMail},
	}))
	rec := httptest.NewRecorder()
	h.register(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusBadRequest)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_request" {
		t.Errorf("error code = %q; want invalid_request", body["error"])
	}
}

// TestRegister_RejectsMissingTokenContext pins that each
// handler defensively rechecks the token context, even though
// the middleware ought to guarantee it. This keeps the handler
// safe against a future routing mistake that mounts the
// handler without the middleware.
func TestRegister_RejectsMissingTokenContext(t *testing.T) {
	h := newHandlersForTest(t)
	req := httptest.NewRequest("POST", "/api/v1/integ/webhooks", strings.NewReader(`{"url":"https://x"}`))
	// no token context attached
	rec := httptest.NewRecorder()
	h.register(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestList_RejectsMissingTokenContext mirrors the previous
// test for the list handler.
func TestList_RejectsMissingTokenContext(t *testing.T) {
	h := newHandlersForTest(t)
	req := httptest.NewRequest("GET", "/api/v1/integ/webhooks", nil)
	rec := httptest.NewRecorder()
	h.list(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestDelete_RejectsMissingTokenContext mirrors the previous
// test for the delete handler.
func TestDelete_RejectsMissingTokenContext(t *testing.T) {
	h := newHandlersForTest(t)
	req := httptest.NewRequest("DELETE", "/api/v1/integ/webhooks/abc", nil)
	rec := httptest.NewRecorder()
	h.del(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestTestFire_RejectsMissingTokenContext mirrors the previous
// test for the test-fire handler.
func TestTestFire_RejectsMissingTokenContext(t *testing.T) {
	h := newHandlersForTest(t)
	req := httptest.NewRequest("POST", "/api/v1/integ/webhooks/abc/test", nil)
	rec := httptest.NewRecorder()
	h.testFire(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestHasAnyIntegrationScope_RejectsEmptyScopeSet pins the
// degenerate case: a token with no scopes at all must not
// pass the boundary.
func TestHasAnyIntegrationScope_RejectsEmptyScopeSet(t *testing.T) {
	if hasAnyIntegrationScope(&oauth.AccessTokenContext{Scopes: nil}) {
		t.Errorf("hasAnyIntegrationScope(nil scopes) = true; want false")
	}
	if hasAnyIntegrationScope(&oauth.AccessTokenContext{Scopes: []string{}}) {
		t.Errorf("hasAnyIntegrationScope([]) = true; want false")
	}
}

// withTokenCtx is a test helper that attaches an
// AccessTokenContext to the request context using the same
// unexported context-key shape that oauth.AuthMiddleware uses
// in production. We can't reach the unexported key directly,
// so we go through oauth.FromContext's tested setter — but
// AuthMiddleware does the WithValue inline. To stay decoupled
// from oauth internals, we exercise the helper via
// oauth.WithAccessToken (if exported) and otherwise wire the
// token through a known-shape call into the package's exported
// surface.
//
// The most stable approach is to drive a real http.Handler
// chain that includes the production AuthMiddleware. But that
// would require a valid Postgres-backed Service; far heavier
// than these unit tests need. Instead, the oauth package
// exports `oauth.WithAccessTokenContext` for exactly this
// scenario.
func withTokenCtx(ctx context.Context, tc *oauth.AccessTokenContext) context.Context {
	return oauth.WithAccessTokenContext(ctx, tc)
}

// newHandlersForTest constructs Handlers wired to a Service
// stub that PANICS on any DB call — these tests only exercise
// the input-validation and middleware paths.
func newHandlersForTest(t *testing.T) *Handlers {
	t.Helper()
	return &Handlers{
		svc:    &Service{cfg: ServiceConfig{}}, // no Pool / Webhooks / OAuth
		logger: log.New(io.Discard, "", 0),
	}
}
