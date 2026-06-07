package featureflags

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// discardLogger is a no-op logger for handler tests that exercise the
// error paths (which log) without polluting test output.
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// fakeStore is an in-memory adminStore for handler tests.
type fakeStore struct {
	flags     map[string]Flag
	overrides map[string]Override // key: scopeKey on the (single) test flag set
	loadErr   error               // when set, loadViews returns it (DB-outage simulation)
}

func newFakeStore() *fakeStore {
	return &fakeStore{flags: map[string]Flag{}, overrides: map[string]Override{}}
}

func ovKey(flag string, scope Scope, id string) string { return flag + "|" + scopeKey(scope, id) }

func (f *fakeStore) loadViews(context.Context) ([]FlagView, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	flags := make([]Flag, 0, len(f.flags))
	for _, fl := range f.flags {
		flags = append(flags, fl)
	}
	overrides := make([]Override, 0, len(f.overrides))
	for _, o := range f.overrides {
		overrides = append(overrides, o)
	}
	return assembleViews(flags, overrides), nil
}

func (f *fakeStore) UpsertFlag(_ context.Context, fl Flag) (*Flag, error) {
	f.flags[fl.Key] = fl
	return &fl, nil
}

func (f *fakeStore) DeleteFlag(_ context.Context, key string) error {
	delete(f.flags, key)
	for k, o := range f.overrides {
		if o.FlagKey == key {
			delete(f.overrides, k)
		}
	}
	return nil
}

func (f *fakeStore) SetOverride(_ context.Context, o Override) (*Override, error) {
	f.overrides[ovKey(o.FlagKey, o.Scope, o.ScopeID)] = o
	return &o, nil
}

func (f *fakeStore) DeleteOverride(_ context.Context, flagKey string, scope Scope, scopeID string) error {
	delete(f.overrides, ovKey(flagKey, scope, scopeID))
	return nil
}

// authed wraps a request context with a test tenant/user so the
// handler's auth gate passes (parity with the OIDC middleware).
func authed(r *http.Request) *http.Request {
	ctx := middleware.WithKChatUserID(middleware.WithTenantID(r.Context(), "tenant-1"), "admin-1")
	return r.WithContext(ctx)
}

func TestHandlerPutCreatesFlagAndOverride(t *testing.T) {
	store := newFakeStore()
	h := &Handlers{store: store, logger: nil}

	body := `{"key":"thread_view","description":"threaded view","default_enabled":false,
	          "overrides":[{"scope":"plan","scope_id":"pro","enabled":true}]}`
	req := authed(httptest.NewRequest(http.MethodPut, "/api/v1/admin/feature-flags", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	h.put(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("put: status %d, body %s", rec.Code, rec.Body.String())
	}
	if _, ok := store.flags["thread_view"]; !ok {
		t.Fatal("flag was not persisted")
	}
	if _, ok := store.overrides[ovKey("thread_view", ScopePlan, "pro")]; !ok {
		t.Fatal("override was not persisted")
	}
	var view FlagView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if view.Key != "thread_view" || len(view.Overrides) != 1 {
		t.Fatalf("unexpected view: %+v", view)
	}
}

func TestHandlerPutRejectsBadScope(t *testing.T) {
	store := newFakeStore()
	h := &Handlers{store: store, logger: nil}
	body := `{"key":"x","overrides":[{"scope":"galaxy","scope_id":"y","enabled":true}]}`
	req := authed(httptest.NewRequest(http.MethodPut, "/api/v1/admin/feature-flags", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	h.put(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad scope, got %d", rec.Code)
	}
	if len(store.flags) != 0 {
		t.Fatal("no flag should be created when an override op is invalid")
	}
}

func TestHandlerPutRequiresKey(t *testing.T) {
	h := &Handlers{store: newFakeStore(), logger: nil}
	req := authed(httptest.NewRequest(http.MethodPut, "/api/v1/admin/feature-flags", bytes.NewBufferString(`{}`)))
	rec := httptest.NewRecorder()
	h.put(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing key, got %d", rec.Code)
	}
}

func TestHandlerPutUnauthenticated(t *testing.T) {
	h := &Handlers{store: newFakeStore(), logger: nil}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/feature-flags", bytes.NewBufferString(`{"key":"x"}`))
	rec := httptest.NewRecorder()
	h.put(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth context, got %d", rec.Code)
	}
}

func TestHandlerDeleteFlag(t *testing.T) {
	store := newFakeStore()
	store.flags["gone"] = Flag{Key: "gone"}
	store.overrides[ovKey("gone", ScopeGlobal, "")] = Override{FlagKey: "gone", Scope: ScopeGlobal}
	h := &Handlers{store: store, logger: nil}

	body := `{"key":"gone","delete":true}`
	req := authed(httptest.NewRequest(http.MethodPut, "/api/v1/admin/feature-flags", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	h.put(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on delete, got %d", rec.Code)
	}
	if _, ok := store.flags["gone"]; ok {
		t.Fatal("flag should be deleted")
	}
	if len(store.overrides) != 0 {
		t.Fatal("overrides should cascade-delete")
	}
}

func TestHandlerList(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a", DefaultEnabled: true}
	h := &Handlers{store: store, logger: nil}
	req := authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil))
	rec := httptest.NewRecorder()
	h.list(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d", rec.Code)
	}
	var resp struct {
		Flags []FlagView `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Flags) != 1 || resp.Flags[0].Key != "a" {
		t.Fatalf("unexpected list: %+v", resp.Flags)
	}
}

// TestHandlerListDBUnavailableReturns503 asserts that a control-plane
// read failing because the DB is unavailable — a read-deadline timeout,
// a cancelled request, or a missing pool — surfaces as a retryable 503
// (not a 500), with a Retry-After hint. This is the user-visible half
// of the chaos-postgres fix: a stalled Postgres now fails fast and
// retryably instead of hanging the request.
func TestHandlerListDBUnavailableReturns503(t *testing.T) {
	cases := map[string]error{
		"deadline": context.DeadlineExceeded,
		"canceled": context.Canceled,
		"no-pool":  ErrNoPool,
		// The Store wraps query errors with fmt.Errorf %w; ensure the
		// 503 classification still unwraps through that chain.
		"wrapped-deadline": wrap(context.DeadlineExceeded),
	}
	for name, loadErr := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()
			store.loadErr = loadErr
			h := &Handlers{store: store, logger: discardLogger()}
			req := authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil))
			rec := httptest.NewRecorder()
			h.list(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got == "" {
				t.Errorf("missing Retry-After header")
			}
		})
	}
}

// wrap mirrors how the Store wraps query errors (fmt.Errorf %w), so the
// test verifies errors.Is unwraps through the Store's error chain.
func wrap(err error) error { return errWrap{err} }

type errWrap struct{ err error }

func (e errWrap) Error() string { return "featureflags: list flags: " + e.err.Error() }
func (e errWrap) Unwrap() error { return e.err }
