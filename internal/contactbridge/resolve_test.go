package contactbridge

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const accountGetBody = `{"methodResponses":[["x:Account/get",{"list":[{"id":"b","name":"kmail-dev","emailAddress":"kmail-dev@kmail.dev"}]},"0"]]}`

// TestDavAccountResolvesEmailAndCaches verifies the JMAP id -> account
// email resolution actually drives the CardDAV path (Stalwart keys DAV
// collections by the account email, not the login name), and that a
// resolved email is memoised (only one x:Account/get call across
// repeated use).
func TestDavAccountResolvesEmailAndCaches(t *testing.T) {
	var jmapCalls int32
	var propfindPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/jmap":
			atomic.AddInt32(&jmapCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(accountGetBody))
		case r.Method == "PROPFIND":
			propfindPath = r.URL.Path
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<multistatus xmlns="DAV:"></multistatus>`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	svc := NewService(Config{StalwartURL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	ctx := context.Background()

	if got := svc.davAccount(ctx, "b"); got != "kmail-dev@kmail.dev" {
		t.Fatalf("davAccount=%q want kmail-dev@kmail.dev", got)
	}
	if got := svc.davAccount(ctx, "b"); got != "kmail-dev@kmail.dev" {
		t.Fatalf("davAccount (cached)=%q want kmail-dev@kmail.dev", got)
	}
	if n := atomic.LoadInt32(&jmapCalls); n != 1 {
		t.Fatalf("x:Account/get calls=%d want 1 (second served from cache)", n)
	}

	// The resolved *email* — not the raw JMAP id — must drive the path.
	if _, err := svc.ListAddressBooks(ctx, "b"); err != nil {
		t.Fatalf("ListAddressBooks: %v", err)
	}
	if propfindPath != "/dav/card/kmail-dev@kmail.dev/" {
		t.Fatalf("PROPFIND path=%q want /dav/card/kmail-dev@kmail.dev/", propfindPath)
	}
}

// TestDavAccountDoesNotCacheFailures guards the regression: a transient
// lookup failure must fall back to the raw id for that call without
// poisoning the cache, so a later call retries and resolves once
// Stalwart recovers.
func TestDavAccountDoesNotCacheFailures(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/jmap" {
			if fail.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(accountGetBody))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	svc := NewService(Config{StalwartURL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	ctx := context.Background()

	if got := svc.davAccount(ctx, "b"); got != "b" {
		t.Fatalf("davAccount on transient failure=%q want raw id b", got)
	}
	fail.Store(false)
	if got := svc.davAccount(ctx, "b"); got != "kmail-dev@kmail.dev" {
		t.Fatalf("davAccount after recovery=%q want kmail-dev@kmail.dev (failure must not be cached)", got)
	}
}

// TestDavAccountIgnoresMismatchedID guards the security hardening: if
// x:Account/get returns an entry whose id does not match the one
// requested (a server bug, or an id-format mismatch), the bridge must
// NOT adopt — let alone cache — that unrelated principal's email. It
// falls back to the raw id so a stray response can never silently
// route one account's CardDAV traffic to another's addressbook home.
func TestDavAccountIgnoresMismatchedID(t *testing.T) {
	// Response carries a different principal ("z"/"someone-else") than
	// the requested id ("b").
	const mismatchBody = `{"methodResponses":[["x:Account/get",{"list":[{"id":"z","name":"someone-else","emailAddress":"someone-else@kmail.dev"}]},"0"]]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/jmap" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mismatchBody))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	svc := NewService(Config{StalwartURL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	ctx := context.Background()

	if got := svc.davAccount(ctx, "b"); got != "b" {
		t.Fatalf("davAccount on id mismatch=%q want raw id b (never another principal's email)", got)
	}
	if _, ok := svc.emailCache.Get("b"); ok {
		t.Fatalf("mismatched lookup must not be cached")
	}
}

// TestDavAccountLogsHardFailure asserts a *hard* resolution failure
// (transport / non-2xx / malformed body) is logged once when a Logger
// is configured, so a misconfigured admin credential or unreachable
// Stalwart is observable instead of silently 404-ing every CardDAV
// call. The call still falls back to the raw id.
func TestDavAccountLogsHardFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/jmap" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	svc := NewService(Config{
		StalwartURL:   srv.URL,
		AdminUser:     "admin",
		AdminPassword: "pw",
		Logger:        log.New(&buf, "", 0),
	})
	if got := svc.davAccount(context.Background(), "b"); got != "b" {
		t.Fatalf("davAccount on hard failure=%q want raw id b", got)
	}
	if !strings.Contains(buf.String(), `account "b"`) {
		t.Fatalf("hard failure must be logged, got %q", buf.String())
	}
}

// TestDavAccountSilentOnCleanNotFound asserts a clean not-found (a
// successful x:Account/get that simply lacks the requested id, e.g. an
// unprovisioned principal) is NOT logged — only hard failures are — so
// an expected, non-alarming outcome doesn't generate log noise.
func TestDavAccountSilentOnCleanNotFound(t *testing.T) {
	const emptyList = `{"methodResponses":[["x:Account/get",{"list":[]},"0"]]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/jmap" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(emptyList))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	svc := NewService(Config{
		StalwartURL:   srv.URL,
		AdminUser:     "admin",
		AdminPassword: "pw",
		Logger:        log.New(&buf, "", 0),
	})
	if got := svc.davAccount(context.Background(), "b"); got != "b" {
		t.Fatalf("davAccount on clean not-found=%q want raw id b", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("clean not-found must not be logged, got %q", buf.String())
	}
}

// TestDavAccountLogsJMAPMethodError asserts a JMAP method-level error
// returned as an HTTP 200 `["error",{...}]` tuple is treated as a hard
// failure: it must be logged (not silently swallowed as a benign
// not-found) and the call still falls back to the raw id.
func TestDavAccountLogsJMAPMethodError(t *testing.T) {
	const jmapErrBody = `{"methodResponses":[["error",{"type":"unknownMethod"},"0"]]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/jmap" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(jmapErrBody))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	svc := NewService(Config{
		StalwartURL:   srv.URL,
		AdminUser:     "admin",
		AdminPassword: "pw",
		Logger:        log.New(&buf, "", 0),
	})
	if got := svc.davAccount(context.Background(), "b"); got != "b" {
		t.Fatalf("davAccount on JMAP method error=%q want raw id b", got)
	}
	if !strings.Contains(buf.String(), "unknownMethod") {
		t.Fatalf("JMAP method error must be logged with its type, got %q", buf.String())
	}
	if _, ok := svc.emailCache.Get("b"); ok {
		t.Fatalf("JMAP method error must not be cached")
	}
}

// TestDevAdminConfig verifies the dev/CI wiring helper: it only layers
// in the Stalwart superuser credentials when both the dev gate is open
// and KMAIL_STALWART_ADMIN_USER is set, and otherwise returns a bare
// config (the production mTLS path).
func TestDevAdminConfig(t *testing.T) {
	logger := log.New(&bytes.Buffer{}, "", 0)

	t.Run("prod env yields bare config", func(t *testing.T) {
		t.Setenv("KMAIL_STALWART_ADMIN_USER", "admin")
		t.Setenv("KMAIL_STALWART_ADMIN_PASS", "pw")
		cfg := DevAdminConfig("http://stalwart:8080", false, logger)
		if cfg.AdminUser != "" || cfg.AdminPassword != "" || cfg.Logger != nil {
			t.Fatalf("prod config must not carry admin creds/logger, got %+v", cfg)
		}
		if cfg.StalwartURL != "http://stalwart:8080" {
			t.Fatalf("StalwartURL=%q", cfg.StalwartURL)
		}
	})

	t.Run("dev env without admin user yields bare config", func(t *testing.T) {
		t.Setenv("KMAIL_STALWART_ADMIN_USER", "")
		cfg := DevAdminConfig("http://stalwart:8080", true, logger)
		if cfg.AdminUser != "" || cfg.Logger != nil {
			t.Fatalf("dev config without admin user must stay bare, got %+v", cfg)
		}
	})

	t.Run("dev env with admin user wires creds and logger", func(t *testing.T) {
		t.Setenv("KMAIL_STALWART_ADMIN_USER", "admin")
		t.Setenv("KMAIL_STALWART_ADMIN_PASS", "pw")
		cfg := DevAdminConfig("http://stalwart:8080", true, logger)
		if cfg.AdminUser != "admin" || cfg.AdminPassword != "pw" || cfg.Logger != logger {
			t.Fatalf("dev config must wire creds + logger, got %+v", cfg)
		}
	})
}

// TestDavAccountNoAdminCreds confirms the bridge issues no management
// call and returns the id unchanged when it holds no credentials (the
// production mTLS path, where the resolver is a no-op).
func TestDavAccountNoAdminCreds(t *testing.T) {
	var jmapCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jmap" {
			atomic.AddInt32(&jmapCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	svc := NewService(Config{StalwartURL: srv.URL})
	if got := svc.davAccount(context.Background(), "b"); got != "b" {
		t.Fatalf("davAccount without creds=%q want b", got)
	}
	if n := atomic.LoadInt32(&jmapCalls); n != 0 {
		t.Fatalf("x:Account/get calls=%d want 0 (no creds → no lookup)", n)
	}
}
