package calendarbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

const accountGetBody = `{"methodResponses":[["x:Account/get",{"list":[{"id":"b","name":"kmail-dev"}]},"0"]]}`

// TestDavAccountResolvesNameAndCaches verifies the JMAP id -> account
// name resolution actually drives the CalDAV path, and that a resolved
// name is memoised (only one x:Account/get call across repeated use).
func TestDavAccountResolvesNameAndCaches(t *testing.T) {
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

	if got := svc.davAccount(ctx, "b"); got != "kmail-dev" {
		t.Fatalf("davAccount=%q want kmail-dev", got)
	}
	if got := svc.davAccount(ctx, "b"); got != "kmail-dev" {
		t.Fatalf("davAccount (cached)=%q want kmail-dev", got)
	}
	if n := atomic.LoadInt32(&jmapCalls); n != 1 {
		t.Fatalf("x:Account/get calls=%d want 1 (second served from cache)", n)
	}

	// The resolved *name* — not the raw JMAP id — must drive the path.
	if _, err := svc.ListCalendars(ctx, "b"); err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if propfindPath != "/dav/cal/kmail-dev/" {
		t.Fatalf("PROPFIND path=%q want /dav/cal/kmail-dev/", propfindPath)
	}
}

// TestDavAccountDoesNotCacheFailures guards the regression the review
// flagged: a transient lookup failure must fall back to the raw id for
// that call without poisoning the cache, so a later call retries and
// resolves once Stalwart recovers.
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
	if got := svc.davAccount(ctx, "b"); got != "kmail-dev" {
		t.Fatalf("davAccount after recovery=%q want kmail-dev (failure must not be cached)", got)
	}
}

// TestDavAccountIgnoresMismatchedID guards the security hardening the
// review flagged: if x:Account/get returns an entry whose id does not
// match the one requested (a server bug, or an id-format mismatch),
// the bridge must NOT adopt — let alone cache — that unrelated
// principal's name. It falls back to the raw id so a stray response
// can never silently route one account's CalDAV traffic to another's
// calendar home.
func TestDavAccountIgnoresMismatchedID(t *testing.T) {
	// Response carries a different principal ("z"/"someone-else") than
	// the requested id ("b").
	const mismatchBody = `{"methodResponses":[["x:Account/get",{"list":[{"id":"z","name":"someone-else"}]},"0"]]}`
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
		t.Fatalf("davAccount on id mismatch=%q want raw id b (never another principal's name)", got)
	}
	if _, ok := svc.nameCache.Get("b"); ok {
		t.Fatalf("mismatched lookup must not be cached")
	}
}

// TestDavAccountNoAdminCreds confirms the bridge issues no management
// call and returns the id unchanged when it holds no credentials
// (the production mTLS path, where the resolver is a no-op).
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
