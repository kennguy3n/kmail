package oauth

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewHandlersAndSetters covers the production NewHandlers
// constructor plus the SetSecureCookies / SetLogger option setters.
func TestNewHandlersAndSetters(t *testing.T) {
	h := NewHandlers(&Service{}, func(*http.Request) (string, string, bool) { return "t", "u", true })
	if h == nil || h.svc == nil {
		t.Fatal("NewHandlers returned an unwired handler")
	}
	h.SetSecureCookies(true)
	if !h.secureCookies {
		t.Error("SetSecureCookies(true) did not stick")
	}
	// SetLogger(nil) must fall back to the default rather than nil-panic.
	h.SetLogger(nil)
	if h.serverLogger() == nil {
		t.Error("serverLogger must never be nil after SetLogger(nil)")
	}
	var buf bytes.Buffer
	h.SetLogger(log.New(&buf, "", 0))
	if h.serverLogger() == nil {
		t.Error("serverLogger nil after explicit SetLogger")
	}
}

// TestWriteTokenError verifies the server-side log gate: a 5xx wire
// error is logged (operator visibility) while a 4xx client mistake is
// not, and both write the RFC 6749 envelope to the client.
func TestWriteTokenError(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandlers(&Service{}, nil)
	h.SetLogger(log.New(&buf, "", 0))

	// A 4xx-class error (invalid grant) — not logged.
	rec := httptest.NewRecorder()
	h.writeTokenError(rec, "/oauth/token", ErrCodeNotFound)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("expected 4xx for ErrCodeNotFound, got %d", rec.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("4xx client error should not be logged, got %q", buf.String())
	}

	// A server-side error → 5xx + logged.
	rec = httptest.NewRecorder()
	h.writeTokenError(rec, "/oauth/token", errors.New("pgx: connection reset"))
	if rec.Code < 500 {
		t.Fatalf("expected 5xx for opaque error, got %d", rec.Code)
	}
	if !strings.Contains(buf.String(), "server-side failure") {
		t.Errorf("5xx error should be logged, got %q", buf.String())
	}
}

// TestRedirectWithOAuthError covers the authorize-error redirect
// mapping: typed OAuth errors and sentinels redirect back to the
// client with an error code, while an invalid redirect URI must NOT
// redirect (spec §3.1.2.4) and instead returns 400.
func TestRedirectWithOAuthError(t *testing.T) {
	h := NewHandlers(&Service{}, nil)
	const redirect = "https://app.example.com/cb"

	// ErrScopeNotAllowed → 302 back to redirect with invalid_scope.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/authorize", nil)
	h.redirectWithOAuthError(rec, req, redirect, "xyz", ErrScopeNotAllowed)
	if rec.Code != http.StatusFound {
		t.Fatalf("scope error: code=%d want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "error=invalid_scope") || !strings.Contains(loc, "state=xyz") {
		t.Errorf("redirect location=%q", loc)
	}

	// ErrInvalidRedirectURI → MUST NOT redirect; 400 instead.
	rec = httptest.NewRecorder()
	h.redirectWithOAuthError(rec, req, redirect, "xyz", ErrInvalidRedirectURI)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid redirect uri: code=%d want 400 (no open redirect)", rec.Code)
	}

	// Opaque error → 302 with server_error.
	rec = httptest.NewRecorder()
	h.redirectWithOAuthError(rec, req, redirect, "", errors.New("boom"))
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "error=server_error") {
		t.Errorf("opaque: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}
