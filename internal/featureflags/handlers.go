package featureflags

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the feature-flag admin HTTP surface:
//
//	GET /api/v1/admin/feature-flags  — list every flag + its overrides
//	PUT /api/v1/admin/feature-flags  — upsert a flag and/or its overrides
//
// Both routes sit behind the OIDC auth middleware (parity with the
// other /api/v1/admin/* surfaces such as adminproxy and scim). The
// store is the source of truth; the resolver's in-memory snapshot is
// Invalidate()d after every write so a rollout change is visible on
// the next IsEnabled rather than after the cache TTL.
type Handlers struct {
	store  adminStore
	svc    *Service
	logger *log.Logger

	// lastGood is the last successful loadViews result, served stale
	// (with staleness headers) when a later read finds Postgres
	// unavailable. This closes the control-plane read gap: rather than
	// returning 503 the moment the DB stalls, the admin GET keeps
	// serving the last-known-good snapshot — the same graceful-
	// degradation guarantee the resolver Service already has for flag
	// evaluation. Writes never read from here; only the GET degrades.
	cacheMu  sync.RWMutex
	lastGood []FlagView
	cachedAt time.Time

	// reconcileWG tracks in-flight asynchronous post-write cache
	// reconciliations (refreshCacheAfterWrite launched off the response
	// path). It lets a graceful shutdown — and the tests — wait for a
	// just-kicked reconcile to finish instead of racing it.
	reconcileWG sync.WaitGroup
}

// adminStore is the write+read surface the handlers need. The pgx
// [Store] is the production implementation; tests inject a fake so the
// HTTP layer can be exercised without a live Postgres.
type adminStore interface {
	loadViews(ctx context.Context) ([]FlagView, error)
	UpsertFlag(ctx context.Context, f Flag) (*Flag, error)
	DeleteFlag(ctx context.Context, key string) error
	SetOverride(ctx context.Context, o Override) (*Override, error)
	DeleteOverride(ctx context.Context, flagKey string, scope Scope, scopeID string) error
}

// NewHandlers builds the admin handlers. svc may be nil (e.g. in a
// binary that exposes the admin API but never resolves flags itself);
// writes then skip cache invalidation.
func NewHandlers(store *Store, svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{store: store, svc: svc, logger: logger}
}

// Register installs the admin routes on the mux behind authMW.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/admin/feature-flags", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("PUT /api/v1/admin/feature-flags", authMW.Wrap(http.HandlerFunc(h.put)))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	views, err := h.store.loadViews(r.Context())
	if err != nil {
		// Postgres is momentarily unavailable. Prefer serving the
		// last-known-good snapshot (clearly marked stale) over a 503:
		// an operator reading flags during a DB blip still sees the
		// real state, and only a cold start (no snapshot yet) degrades
		// to the retryable error.
		if isStoreUnavailable(err) {
			if cached, age, ok := h.cachedViews(); ok {
				if h.logger != nil {
					h.logger.Printf("featureflags: store unavailable (%v); serving snapshot aged %s", err, age.Round(time.Second))
				}
				writeStaleJSON(w, age, map[string]any{"flags": cached})
				return
			}
		}
		h.writeStoreErr(w, err)
		return
	}
	h.cacheViews(views)
	writeJSON(w, http.StatusOK, map[string]any{"flags": views})
}

// cacheViews records the latest successful read as the last-known-good
// snapshot served during a subsequent DB outage.
func (h *Handlers) cacheViews(views []FlagView) {
	h.cacheMu.Lock()
	h.lastGood = views
	h.cachedAt = time.Now()
	h.cacheMu.Unlock()
}

// cachedViews returns the last-known-good snapshot and its age, or
// ok=false if nothing has been cached yet (cold start).
func (h *Handlers) cachedViews() (views []FlagView, age time.Duration, ok bool) {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	if h.lastGood == nil {
		return nil, 0, false
	}
	return h.lastGood, time.Since(h.cachedAt), true
}

// dropCache clears the last-known-good snapshot so the next read during
// an outage degrades to a retryable 503 rather than serving data that a
// confirmed write has already contradicted.
func (h *Handlers) dropCache() {
	h.cacheMu.Lock()
	h.lastGood = nil
	h.cachedAt = time.Time{}
	h.cacheMu.Unlock()
}

// removeFlagFromCache reconciles the stale-serving cache with a
// just-committed whole-flag delete by dropping that flag's view from
// the snapshot in place (copy-on-write swap), without a DB round-trip.
// A delete's effect on the cache is fully known from the key alone, so
// there's no reason to reload: this keeps the delete response from
// blocking on a reconciliation read and makes the reconciliation immune
// to a client disconnect. Other entries stay as stale as the last full
// read, so cachedAt is intentionally left unchanged. A nil (cold) cache
// stays cold.
func (h *Handlers) removeFlagFromCache(key string) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if h.lastGood == nil {
		return
	}
	filtered := make([]FlagView, 0, len(h.lastGood))
	for _, v := range h.lastGood {
		if v.Key != key {
			filtered = append(filtered, v)
		}
	}
	h.lastGood = filtered
}

// cacheRefreshBudget bounds the post-write cache reconciliation read.
// It mirrors the Store's default read deadline so the refresh fails
// fast against a stalled DB even if the Store was built without its own
// timeout; the Store's own readContext bounds it tighter when set.
const cacheRefreshBudget = 5 * time.Second

// refreshCacheAfterWrite reconciles the stale-serving cache with a
// just-committed flag upsert whose convenience reload (the one that
// builds the response body) failed. It reloads the view set and
// re-caches it on success; if that reload also fails (e.g. the DB went
// unavailable right after the write), it drops the cache so a
// subsequent outage read returns 503 instead of serving a snapshot that
// no longer matches the committed state.
//
// The reconciliation read is deliberately decoupled from the caller's
// request context: it keeps the request's values (tracing, etc.) but
// drops its cancellation/deadline and applies its own budget. The most
// common reason the convenience reload fails is the client
// disconnecting — decoupling lets the reconciliation still succeed on
// the server's terms and re-cache, rather than a mere disconnect
// needlessly dropping a usable snapshot (and degrading a later outage
// read to 503). The write already committed; finishing the read here is
// correct. Whole-flag deletes don't use this path: their cache effect
// is fully known from the key, so removeFlagFromCache reconciles them
// in memory without a reload.
//
// Callers run this off the response path via reconcileCacheAfterWriteAsync
// so the write isn't blocked by the (up to cacheRefreshBudget) read.
func (h *Handlers) refreshCacheAfterWrite(parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cacheRefreshBudget)
	defer cancel()
	views, err := h.store.loadViews(ctx)
	if err != nil {
		if h.logger != nil {
			h.logger.Printf("featureflags: cache refresh after write failed (%v); dropping stale snapshot", err)
		}
		h.dropCache()
		return
	}
	h.cacheViews(views)
}

// reconcileCacheAfterWriteAsync runs refreshCacheAfterWrite in the
// background so the write response returns immediately rather than
// blocking up to cacheRefreshBudget on the reconciliation read. The
// read is already decoupled from the request context (its own budget),
// so it completes correctly after the handler returns; reconcileWG lets
// shutdown and tests await it. Until it finishes, the cache still holds
// the pre-reconcile snapshot — the same in-flight window the synchronous
// version had, well within the system's bounded-staleness model — and it
// converges to the committed state (or an empty cache → 503) once done.
func (h *Handlers) reconcileCacheAfterWriteAsync(parent context.Context) {
	detached := context.WithoutCancel(parent)
	h.reconcileWG.Add(1)
	go func() {
		defer h.reconcileWG.Done()
		h.refreshCacheAfterWrite(detached)
	}()
}

// overrideOp is one override mutation in a PUT body. Delete=true
// removes the (scope, scope_id) override; otherwise it is upserted with
// Enabled.
type overrideOp struct {
	Scope   Scope  `json:"scope"`
	ScopeID string `json:"scope_id"`
	Enabled bool   `json:"enabled"`
	Delete  bool   `json:"delete"`
}

// putRequest upserts a flag definition and applies zero or more
// override mutations atomically from the caller's perspective (each
// store op is its own statement; the snapshot is invalidated once at
// the end). Setting Delete=true with only Key removes the whole flag
// (and, via ON DELETE CASCADE, its overrides).
type putRequest struct {
	Key            string       `json:"key"`
	Description    string       `json:"description"`
	DefaultEnabled bool         `json:"default_enabled"`
	Delete         bool         `json:"delete"`
	Overrides      []overrideOp `json:"overrides"`
}

func (h *Handlers) put(w http.ResponseWriter, r *http.Request) {
	if middleware.KChatUserIDFrom(r.Context()) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	var in putRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if in.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key required"})
		return
	}

	// Whole-flag delete short-circuit.
	if in.Delete {
		if err := h.store.DeleteFlag(r.Context(), in.Key); err != nil {
			h.writeStoreErr(w, err)
			return
		}
		h.invalidate()
		// Keep the stale-serving cache consistent with the delete so a
		// later outage read can't resurface the removed flag. The cache
		// effect is fully known from the key, so reconcile in memory —
		// no reload, so the response isn't blocked and a client
		// disconnect can't affect it.
		h.removeFlagFromCache(in.Key)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Validate every override op up front so a bad op fails the whole
	// request before we mutate anything (closest we get to atomicity
	// without a single multi-statement tx, and good enough since the
	// flag upsert is idempotent).
	for _, op := range in.Overrides {
		ov := Override{FlagKey: in.Key, Scope: op.Scope, ScopeID: op.ScopeID, Enabled: op.Enabled}
		if op.Delete {
			// Delete only needs a valid scope/scope_id shape.
			if _, ok := validScopes[op.Scope]; !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scope: " + string(op.Scope)})
				return
			}
			continue
		}
		if err := ov.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}

	// Invalidate the resolver snapshot if ANY write lands, even when a
	// later op fails partway through. Otherwise a partial write would
	// leave the in-memory snapshot reflecting pre-write state until the
	// TTL elapsed — surprising for an operator who just made a change
	// and got an error. The deferred call runs on every return path
	// below once `mutated` is set.
	//
	// The stale-serving cache gets the same treatment: if a write lands
	// but we bail out before reconciling the cache with the new state
	// (e.g. a later override op fails), drop it so a subsequent outage
	// read can't serve the pre-write snapshot. The success and
	// reload-failure paths set cacheReconciled once they've refreshed or
	// deliberately dropped it, so this only fires on the partial-write
	// error paths.
	var mutated bool
	var cacheReconciled bool
	defer func() {
		if mutated {
			h.invalidate()
			if !cacheReconciled {
				h.dropCache()
			}
		}
	}()

	if _, err := h.store.UpsertFlag(r.Context(), Flag{
		Key:            in.Key,
		Description:    in.Description,
		DefaultEnabled: in.DefaultEnabled,
	}); err != nil {
		h.writeStoreErr(w, err)
		return
	}
	mutated = true

	for _, op := range in.Overrides {
		if op.Delete {
			if err := h.store.DeleteOverride(r.Context(), in.Key, op.Scope, op.ScopeID); err != nil {
				h.writeStoreErr(w, err)
				return
			}
			continue
		}
		if _, err := h.store.SetOverride(r.Context(), Override{
			FlagKey: in.Key,
			Scope:   op.Scope,
			ScopeID: op.ScopeID,
			Enabled: op.Enabled,
		}); err != nil {
			h.writeStoreErr(w, err)
			return
		}
	}

	// Return the resulting view for the flag so the admin UI can render
	// the post-write state without a follow-up GET.
	views, err := h.store.loadViews(r.Context())
	if err != nil {
		// The write already succeeded; only the convenience reload
		// failed, so still return 200 with the key. Guard the logger to
		// match writeStoreErr (a directly-constructed Handlers may have
		// a nil logger).
		if h.logger != nil {
			h.logger.Printf("featureflags: put reload: %v", err)
		}
		// The write landed but the convenience reload (on the client's
		// context) failed — most often a client disconnect. Reconcile
		// the cache out of band on a decoupled context, off the response
		// path so this PUT isn't blocked by the reconciliation read: a
		// transient disconnect still re-caches the committed state, and
		// only a genuine outage drops the snapshot (→ later read 503).
		// Either way the cache converges to a snapshot the write didn't
		// contradict.
		h.reconcileCacheAfterWriteAsync(r.Context())
		cacheReconciled = true
		writeJSON(w, http.StatusOK, map[string]string{"key": in.Key})
		return
	}
	h.cacheViews(views)
	cacheReconciled = true
	for _, v := range views {
		if v.Key == in.Key {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": in.Key})
}

// invalidate drops the resolver snapshot if a Service is wired.
func (h *Handlers) invalidate() {
	if h.svc != nil {
		h.svc.Invalidate()
	}
}

// isStoreUnavailable reports whether err means the control plane is
// momentarily unavailable (a missing pool, a read that hit its
// deadline, a cancelled request, or a connection-level failure) rather
// than a malformed request or a genuine bug. These are retryable and,
// for reads, eligible for the stale-snapshot fallback.
func isStoreUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// A server-side error response (constraint violation, bad query,
	// undefined column, …) means Postgres is up and answering: that's a
	// genuine error, never eligible for the stale fallback even if it
	// is wrapped alongside a transient-looking cause.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return false
	}
	// Explicit unavailability signals: the read hit its deadline, the
	// request was cancelled, or no pool is wired.
	if errors.Is(err, ErrNoPool) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) {
		return true
	}
	// A connection-level failure that surfaced before the read deadline
	// fired — refused/reset/no-route (net.Error), a closed pool, or a
	// dropped connection (EOF) — is also unavailability, not a data
	// bug, so a warm reader still gets the last-known-good snapshot.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// puddle.ErrClosedPool is what pgxpool.Acquire returns once the pool
	// has been closed (e.g. during graceful shutdown or a pool reset);
	// it is distinct from net.ErrClosed ("use of closed network
	// connection") and would otherwise fall through to a 500.
	return errors.Is(err, puddle.ErrClosedPool) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF)
}

func (h *Handlers) writeStoreErr(w http.ResponseWriter, err error) {
	// Surface a momentarily-unavailable control plane as a retryable
	// 503 (with Retry-After) instead of a 500 so callers and the chaos
	// harness see a fast, honest "try again", not a hang.
	if isStoreUnavailable(err) {
		if h.logger != nil {
			h.logger.Printf("featureflags: store unavailable: %v", err)
		}
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if h.logger != nil {
		h.logger.Printf("featureflags: store error: %v", err)
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// writeStaleJSON serves a last-known-good payload during a DB outage,
// tagged so a client can tell it is not fresh: a standard `Warning:
// 110` ("Response is Stale"), an `Age` in whole seconds, and an
// explicit `X-Kmail-Stale` flag. Status stays 200 — the data is real,
// just not guaranteed current.
func writeStaleJSON(w http.ResponseWriter, age time.Duration, payload any) {
	secs := int(age.Seconds())
	if secs < 0 {
		secs = 0
	}
	w.Header().Set("Age", strconv.Itoa(secs))
	w.Header().Set("Warning", `110 - "Response is Stale"`)
	w.Header().Set("X-Kmail-Stale", "true")
	writeJSON(w, http.StatusOK, payload)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
