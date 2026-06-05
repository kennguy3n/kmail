package featureflags

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// fakeStore is an in-memory adminStore for handler tests.
type fakeStore struct {
	flags     map[string]Flag
	overrides map[string]Override // key: scopeKey on the (single) test flag set
}

func newFakeStore() *fakeStore {
	return &fakeStore{flags: map[string]Flag{}, overrides: map[string]Override{}}
}

func ovKey(flag string, scope Scope, id string) string { return flag + "|" + scopeKey(scope, id) }

func (f *fakeStore) loadViews(context.Context) ([]FlagView, error) {
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
