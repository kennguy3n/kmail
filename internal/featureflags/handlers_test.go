package featureflags

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// discardLogger is a no-op logger for handler tests that exercise the
// error paths (which log) without polluting test output.
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// fakeStore is an in-memory adminStore for handler tests. Its mutable
// state is guarded by mu so it stays race-free even when a test triggers
// an asynchronous post-write reconcile (reconcileCacheAfterWriteAsync)
// that calls loadViews from a background goroutine. Tests arming a
// failure that the background reconcile will observe use setLoadErr so
// the write is ordered behind the same lock.
type fakeStore struct {
	mu          sync.Mutex
	flags       map[string]Flag
	overrides   map[string]Override // key: scopeKey on the (single) test flag set
	loadErr     error               // when set, loadViews returns it (DB-outage simulation)
	loadErrOnce bool                // when true, loadErr fires once then clears (client-disconnect-then-recover)
	overrideErr error               // when set, SetOverride returns it (partial-write simulation)
}

func newFakeStore() *fakeStore {
	return &fakeStore{flags: map[string]Flag{}, overrides: map[string]Override{}}
}

// setLoadErr arms loadViews to fail with err. once=true clears it after
// a single failure (client-disconnect-then-recover simulation).
func (f *fakeStore) setLoadErr(err error, once bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadErr = err
	f.loadErrOnce = once
}

func ovKey(flag string, scope Scope, id string) string { return flag + "|" + scopeKey(scope, id) }

func (f *fakeStore) loadViews(context.Context) ([]FlagView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		err := f.loadErr
		if f.loadErrOnce {
			f.loadErr = nil
		}
		return nil, err
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flags[fl.Key] = fl
	return &fl, nil
}

func (f *fakeStore) DeleteFlag(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.flags, key)
	for k, o := range f.overrides {
		if o.FlagKey == key {
			delete(f.overrides, k)
		}
	}
	return nil
}

func (f *fakeStore) SetOverride(_ context.Context, o Override) (*Override, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.overrideErr != nil {
		return nil, f.overrideErr
	}
	f.overrides[ovKey(o.FlagKey, o.Scope, o.ScopeID)] = o
	return &o, nil
}

func (f *fakeStore) DeleteOverride(_ context.Context, flagKey string, scope Scope, scopeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// TestHandlerListServesStaleSnapshotWhenDBUnavailable asserts the
// cached-read fallback: once a GET has succeeded, a later GET that finds
// Postgres unavailable serves the last-known-good snapshot (200) tagged
// stale instead of returning 503. This is the control-plane analogue of
// the resolver's in-memory snapshot, closing the chaos-postgres
// "served = 0%" gap.
func TestHandlerListServesStaleSnapshotWhenDBUnavailable(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a", DefaultEnabled: true}
	h := &Handlers{store: store, logger: discardLogger()}

	// First read succeeds and warms the last-known-good cache.
	warm := authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil))
	h.list(httptest.NewRecorder(), warm)

	// Now Postgres goes away mid-flight.
	store.loadErr = context.DeadlineExceeded
	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale snapshot), body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Kmail-Stale"); got != "true" {
		t.Errorf("X-Kmail-Stale = %q, want \"true\"", got)
	}
	if got := rec.Header().Get("Warning"); got == "" {
		t.Errorf("missing Warning header on stale response")
	}
	if _, ok := rec.Header()["Age"]; !ok {
		t.Errorf("missing Age header on stale response")
	}
	var resp struct {
		Flags []FlagView `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Flags) != 1 || resp.Flags[0].Key != "a" {
		t.Fatalf("stale body did not carry the cached flag: %+v", resp.Flags)
	}
}

// TestHandlerListGenuineErrorNotServedStale asserts the fallback is
// scoped to transient unavailability only: a non-retryable store error
// still surfaces as 500 even when a warm snapshot exists, so real bugs
// are never masked by stale data.
func TestHandlerListGenuineErrorNotServedStale(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a"}
	h := &Handlers{store: store, logger: discardLogger()}
	h.list(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	store.loadErr = errors.New("scan: malformed row")
	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a genuine error", rec.Code)
	}
	if got := rec.Header().Get("X-Kmail-Stale"); got != "" {
		t.Errorf("genuine error must not be served stale; got X-Kmail-Stale=%q", got)
	}
}

// TestHandlerListServesStaleOnConnectionDrop asserts the fallback also
// covers connection-level failures that surface before the read
// deadline fires (e.g. connection refused/reset), not just
// DeadlineExceeded — a warm reader still gets the last-known-good
// snapshot instead of a 500.
func TestHandlerListServesStaleOnConnectionDrop(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a", DefaultEnabled: true}
	h := &Handlers{store: store, logger: discardLogger()}
	h.list(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	// A dial failure wrapped the way pgx surfaces an unreachable DB.
	store.loadErr = fmt.Errorf("connect: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale snapshot on connection drop), body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Kmail-Stale"); got != "true" {
		t.Errorf("X-Kmail-Stale = %q, want \"true\"", got)
	}
}

// TestHandlerListServesStaleOnClosedPool asserts that puddle.ErrClosedPool
// (what pgxpool.Acquire returns once the pool is closed, e.g. during a
// pool reset) is classified as unavailability and served stale, not as a
// genuine 500 — it is distinct from net.ErrClosed.
func TestHandlerListServesStaleOnClosedPool(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a", DefaultEnabled: true}
	h := &Handlers{store: store, logger: discardLogger()}
	h.list(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	store.loadErr = fmt.Errorf("acquire: %w", puddle.ErrClosedPool)
	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale snapshot on closed pool), body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Kmail-Stale"); got != "true" {
		t.Errorf("X-Kmail-Stale = %q, want \"true\"", got)
	}
}

// TestHandlerListMaxStaleAge asserts the configurable staleness bound:
// a snapshot within maxStaleAge is served stale, but once it ages past
// the bound the read degrades to a retryable 503 instead of returning
// arbitrarily old data.
func TestHandlerListMaxStaleAge(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a", DefaultEnabled: true}
	h := (&Handlers{store: store, logger: discardLogger()}).WithMaxStaleAge(time.Minute)

	// Warm the cache, then take Postgres down.
	h.list(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	store.setLoadErr(context.DeadlineExceeded, false)

	// Fresh snapshot (well within the bound) → served stale.
	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh snapshot: status = %d, want 200 (served stale), body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Kmail-Stale"); got != "true" {
		t.Errorf("X-Kmail-Stale = %q, want \"true\"", got)
	}

	// Age the snapshot past the bound → degrade to 503.
	h.cacheMu.Lock()
	h.cachedAt = h.cachedAt.Add(-2 * time.Minute)
	h.cacheMu.Unlock()
	rec = httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-aged snapshot: status = %d, want 503 (too stale to serve)", rec.Code)
	}
	if got := rec.Header().Get("X-Kmail-Stale"); got != "" {
		t.Errorf("over-aged snapshot must not be served stale; got X-Kmail-Stale=%q", got)
	}
}

// TestHandlerCacheWriteOrdering asserts the monotonic read-start guard:
// a slower reconcile that *started* earlier can neither overwrite a
// snapshot from a later-started read nor wipe it with a stale drop. This
// keeps concurrent admin writes (each kicking an async reconcile) from
// regressing the cache to an older state.
func TestHandlerCacheWriteOrdering(t *testing.T) {
	h := &Handlers{store: newFakeStore(), logger: discardLogger()}
	older := []FlagView{{Flag: Flag{Key: "old"}}}
	newer := []FlagView{{Flag: Flag{Key: "new"}}}
	t0 := time.Now()
	t1 := t0.Add(time.Second)

	// A later-started read wins even if it lands first.
	h.cacheViews(newer, t1)
	h.cacheViews(older, t0) // earlier read-start → rejected
	if got, _, _ := h.cachedViews(); len(got) != 1 || got[0].Key != "new" {
		t.Fatalf("stale reconcile clobbered newer snapshot: got %+v", got)
	}

	// An earlier-started drop must not wipe the newer snapshot.
	h.dropCache(t0)
	if _, _, ok := h.cachedViews(); !ok {
		t.Fatal("stale drop wiped a newer snapshot")
	}

	// A later drop does clear it.
	h.dropCache(t1.Add(time.Second))
	if _, _, ok := h.cachedViews(); ok {
		t.Fatal("fresh drop did not clear the cache")
	}
}

// TestHandlerListServerErrorNotServedStale asserts a server-side SQL
// error (pgconn.PgError — Postgres is up and answering) is treated as a
// genuine 500 and never served from the stale cache, even though it is
// a database error.
func TestHandlerListServerErrorNotServedStale(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a"}
	h := &Handlers{store: store, logger: discardLogger()}
	h.list(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	store.loadErr = fmt.Errorf("list flags: %w", &pgconn.PgError{Code: "42703", Message: "column does not exist"})
	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a server-side SQL error", rec.Code)
	}
	if got := rec.Header().Get("X-Kmail-Stale"); got != "" {
		t.Errorf("server-side error must not be served stale; got X-Kmail-Stale=%q", got)
	}
}

// TestHandlerDeleteReconcilesStaleCacheInMemory asserts that a
// confirmed DELETE reconciles the stale-serving cache in memory: the
// removed flag must not resurface in a later stale read, while the other
// warmed flags survive. The reconciliation needs no DB reload, so it
// holds even if Postgres is already unavailable for reads (and is immune
// to a client disconnect).
func TestHandlerDeleteReconcilesStaleCacheInMemory(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a", DefaultEnabled: true}
	store.flags["b"] = Flag{Key: "b", DefaultEnabled: false}
	h := &Handlers{store: store, logger: discardLogger()}

	// Warm the cache with both flags present.
	h.list(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	// Reads are already failing, but the delete write still lands and
	// reconciles the cache in memory — no reload needed.
	store.loadErr = context.DeadlineExceeded
	del := authed(httptest.NewRequest(http.MethodPut, "/api/v1/admin/feature-flags", bytes.NewBufferString(`{"key":"a","delete":true}`)))
	delRec := httptest.NewRecorder()
	h.put(delRec, del)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d, body %s", delRec.Code, delRec.Body.String())
	}

	// The stale read must drop "a" but keep "b".
	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale snapshot), body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Flags []FlagView `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawB bool
	for _, v := range resp.Flags {
		if v.Key == "a" {
			t.Fatalf("deleted flag resurfaced in stale read: %+v", resp.Flags)
		}
		if v.Key == "b" {
			sawB = true
		}
	}
	if !sawB {
		t.Fatalf("surviving flag \"b\" missing from stale read: %+v", resp.Flags)
	}
}

// TestHandlerPutReloadFailureReconcilesDecoupled asserts that when an
// upsert's convenience reload fails only because the client's context
// went away (disconnect), the decoupled post-write reconcile still
// re-caches the committed state rather than dropping the snapshot — so a
// later outage read serves the new value, not a 503.
func TestHandlerPutReloadFailureReconcilesDecoupled(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a", Description: "old", DefaultEnabled: true}
	h := &Handlers{store: store, logger: discardLogger()}

	// Warm the cache.
	h.list(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	// The writes land; the convenience reload fails once (as a client
	// disconnect would), but the decoupled reconcile read recovers.
	// Use the guarded setter: the reconcile reads loadErr from a
	// background goroutine.
	store.setLoadErr(context.Canceled, true)
	body := `{"key":"a","description":"new","default_enabled":false}`
	wrec := httptest.NewRecorder()
	h.put(wrec, authed(httptest.NewRequest(http.MethodPut, "/api/v1/admin/feature-flags", bytes.NewBufferString(body))))
	if wrec.Code != http.StatusOK {
		t.Fatalf("put: status %d, want 200, body %s", wrec.Code, wrec.Body.String())
	}
	// The reconcile runs off the response path; await it before asserting.
	h.reconcileWG.Wait()

	// A later outage read must serve the reconciled (new) state, not 503.
	store.setLoadErr(context.DeadlineExceeded, false)
	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reconciled stale snapshot), body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Flags []FlagView `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, v := range resp.Flags {
		if v.Key == "a" {
			found = true
			if v.Description != "new" || v.DefaultEnabled {
				t.Fatalf("stale read did not reflect the committed upsert: %+v", v)
			}
		}
	}
	if !found {
		t.Fatalf("flag \"a\" missing from reconciled stale read: %+v", resp.Flags)
	}
}

// TestHandlerPutReloadFailureDropsCacheOnOutage asserts that when an
// upsert's writes land but both the convenience reload and the decoupled
// reconcile fail (a genuine DB outage), the stale cache is dropped so a
// later outage read returns the retryable 503 instead of contradicted
// data.
func TestHandlerPutReloadFailureDropsCacheOnOutage(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a", DefaultEnabled: true}
	h := &Handlers{store: store, logger: discardLogger()}

	h.list(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	// Writes land, but every reload (convenience + decoupled) fails.
	store.setLoadErr(context.DeadlineExceeded, false)
	body := `{"key":"a","default_enabled":false}`
	wrec := httptest.NewRecorder()
	h.put(wrec, authed(httptest.NewRequest(http.MethodPut, "/api/v1/admin/feature-flags", bytes.NewBufferString(body))))
	if wrec.Code != http.StatusOK {
		t.Fatalf("put: status %d, want 200, body %s", wrec.Code, wrec.Body.String())
	}
	// The reconcile runs off the response path; await it before asserting.
	h.reconcileWG.Wait()

	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (cache dropped, no stale snapshot to serve)", rec.Code)
	}
	if got := rec.Header().Get("X-Kmail-Stale"); got != "" {
		t.Errorf("must not serve stale after cache drop; got X-Kmail-Stale=%q", got)
	}
}

// TestHandlerPutPartialWriteDropsStaleCache asserts that when a PUT
// lands a flag upsert but a later override write fails, the
// stale-serving cache is dropped rather than left holding the pre-write
// snapshot — otherwise a subsequent outage read could serve a flag
// state that no committed write matches. A drop is the safe outcome: a
// later outage read returns 503 instead of stale-wrong data.
func TestHandlerPutPartialWriteDropsStaleCache(t *testing.T) {
	store := newFakeStore()
	store.flags["a"] = Flag{Key: "a", DefaultEnabled: true}
	h := &Handlers{store: store, logger: discardLogger()}

	// Warm the cache.
	h.list(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))

	// Upsert succeeds, the override write fails partway through.
	store.overrideErr = errors.New("write conflict")
	body := `{"key":"a","default_enabled":false,"overrides":[{"scope":"plan","scope_id":"pro","enabled":true}]}`
	wrec := httptest.NewRecorder()
	h.put(wrec, authed(httptest.NewRequest(http.MethodPut, "/api/v1/admin/feature-flags", bytes.NewBufferString(body))))
	if wrec.Code != http.StatusInternalServerError {
		t.Fatalf("put: status %d, want 500 on partial-write failure, body %s", wrec.Code, wrec.Body.String())
	}

	// A later outage read must NOT serve the pre-write snapshot.
	store.loadErr = context.DeadlineExceeded
	rec := httptest.NewRecorder()
	h.list(rec, authed(httptest.NewRequest(http.MethodGet, "/api/v1/admin/feature-flags", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (cache dropped after partial write)", rec.Code)
	}
	if got := rec.Header().Get("X-Kmail-Stale"); got != "" {
		t.Errorf("must not serve stale after partial-write cache drop; got X-Kmail-Stale=%q", got)
	}
}

// wrap mirrors how the Store wraps query errors (fmt.Errorf %w), so the
// test verifies errors.Is unwraps through the Store's error chain.
func wrap(err error) error { return errWrap{err} }

type errWrap struct{ err error }

func (e errWrap) Error() string { return "featureflags: list flags: " + e.err.Error() }
func (e errWrap) Unwrap() error { return e.err }
