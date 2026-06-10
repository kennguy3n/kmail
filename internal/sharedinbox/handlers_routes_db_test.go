package sharedinbox

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// siHarness wires the workflow Handlers behind a dev-bypass OIDC
// middleware and returns a mux + a request driver so the Register
// route table (and the auth.Wrap path) is exercised end-to-end.
func siHarness(t *testing.T, svc *WorkflowService) *http.ServeMux {
	t.Helper()
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	NewHandlers(svc, log.New(io.Discard, "", 0)).Register(mux, authMW)
	return mux
}

func siRoute(t *testing.T, mux *http.ServeMux, tenant, kchatUser, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer dev-token")
	r.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	if kchatUser != "" {
		r.Header.Set("X-KMail-Dev-Kchat-User-Id", kchatUser)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

// TestSharedInboxRoutesEndToEnd drives the registered routes through
// the real mux + auth wrapper, including the principal() fallback when
// a note omits author_user_id.
func TestSharedInboxRoutesEndToEnd(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(pool, log.New(io.Discard, "", 0))
	inbox, user := seedInboxAndUser(t, svc, tenant)
	mux := siHarness(t, svc)

	base := "/api/v1/shared-inboxes/" + inbox
	emailBase := base + "/emails/email-e2e"

	// listAssignments via the mux (exercises Register + auth.Wrap).
	if rec := siRoute(t, mux, tenant, user, http.MethodGet, base+"/assignments", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET assignments = %d body=%s", rec.Code, rec.Body.String())
	}

	// addNote without author_user_id → principal() resolves from the
	// X-KMail-Dev-Kchat-User-Id header (a users.id uuid here).
	rec := siRoute(t, mux, tenant, user, http.MethodPost, emailBase+"/notes", `{"note_text":"from principal"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST note (principal) = %d body=%s", rec.Code, rec.Body.String())
	}

	// listNotes via the mux.
	if rec := siRoute(t, mux, tenant, user, http.MethodGet, emailBase+"/notes", ""); rec.Code != http.StatusOK {
		t.Errorf("GET notes = %d", rec.Code)
	}

	// mls/status via the mux (no MLS manager → enabled:false).
	if rec := siRoute(t, mux, tenant, user, http.MethodGet, base+"/mls/status", ""); rec.Code != http.StatusOK {
		t.Errorf("GET mls/status = %d", rec.Code)
	}
}

// TestAddNoteValidationError exercises the ErrInvalidInput → 400 path
// (empty note text) through the service layer.
func TestAddNoteValidationError(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(pool, log.New(io.Discard, "", 0))
	inbox, user := seedInboxAndUser(t, svc, tenant)
	mux := siHarness(t, svc)

	emailBase := "/api/v1/shared-inboxes/" + inbox + "/emails/email-x"
	rec := siRoute(t, mux, tenant, "", http.MethodPost, emailBase+"/notes",
		`{"author_user_id":"`+user+`","note_text":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty note text = %d want 400 body=%s", rec.Code, rec.Body.String())
	}
}
