// Package featureflags is the PostgreSQL-backed feature-flag system
// for the KMail control plane (WS4 Task 1). It is the rollout-safety
// substrate the other workstreams depend on: a workstream gates its
// new code behind `featureflags.IsEnabled(ctx, "thread_view")` and an
// operator dials the flag on per-plan / per-tenant / per-user without
// a redeploy.
//
// # Evaluation model
//
// A flag has a registry default (`feature_flags.default_enabled`) and
// any number of scoped overrides (`feature_flag_overrides`). For a
// given request the most specific matching override wins:
//
//	user > tenant > plan > global > flag default
//
// An *unregistered* flag resolves to false. This is the fail-safe
// posture: a typo in a flag key, or a flag a workstream has not yet
// declared, leaves the gated feature OFF rather than silently ON.
//
// # Caching
//
// Resolving a flag on the hot request path must not hit Postgres, so
// the [Service] keeps an in-memory snapshot of every flag + override
// refreshed on a TTL (and eagerly on admin writes via [Service.Invalidate]).
// The snapshot is tiny (flags are operator-managed, not per-tenant
// data) so loading the whole set is cheap. Tenant→plan lookups are
// cached separately with the same TTL.
//
// # Construction is backend-free
//
// [NewService] performs no I/O — it only stashes its dependencies. The
// first [Service.IsEnabled] / [Service.Snapshot] call loads the
// snapshot lazily, and [Service.Run] refreshes it in the background.
// This keeps the service usable from cmd/kmail-worker's buildWorkers,
// whose invariant forbids Postgres queries at construction time.
package featureflags

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Scope identifies the audience a [Override] targets. The four scopes
// are ordered from least to most specific in [scopePrecedence].
type Scope string

const (
	// ScopeGlobal forces a flag on/off for everyone, overriding the
	// registry default. Its override row always has an empty ScopeID.
	ScopeGlobal Scope = "global"
	// ScopePlan targets a billing plan: "core", "pro", or "privacy".
	ScopePlan Scope = "plan"
	// ScopeTenant targets a single tenant by UUID.
	ScopeTenant Scope = "tenant"
	// ScopeUser targets a single KChat user id.
	ScopeUser Scope = "user"
)

// validScopes is the allowlist enforced by [Override.Validate]; it
// mirrors the CHECK constraint in migration 006.
var validScopes = map[Scope]struct{}{
	ScopeGlobal: {},
	ScopePlan:   {},
	ScopeTenant: {},
	ScopeUser:   {},
}

// defaultCacheTTL bounds how long a loaded snapshot is served before a
// refresh. Feature flags are operator-managed and change rarely, so a
// 30s window trades a small propagation delay for near-zero Postgres
// load on the hot path. Admin writes call Invalidate to bypass it.
const defaultCacheTTL = 30 * time.Second

// Flag is a registry entry: a known flag key plus the value used when
// no override matches.
type Flag struct {
	Key            string    `json:"key"`
	Description    string    `json:"description"`
	DefaultEnabled bool      `json:"default_enabled"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

// Override is a scoped rollout rule forcing a flag on/off for one
// audience.
type Override struct {
	ID        string    `json:"id,omitempty"`
	FlagKey   string    `json:"flag_key,omitempty"`
	Scope     Scope     `json:"scope"`
	ScopeID   string    `json:"scope_id"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Subject is the audience a flag is evaluated against. An empty field
// simply means that scope's overrides cannot match.
type Subject struct {
	TenantID string
	UserID   string
	Plan     string
}

// source is the persistence dependency the Service resolves against.
// The pgx-backed [Store] is the production implementation; tests inject
// an in-memory fake. Keeping this interface tiny is what lets the
// evaluation + caching logic be unit-tested without a live Postgres.
type source interface {
	// loadAll returns the full flag registry and every override.
	loadAll(ctx context.Context) ([]Flag, []Override, error)
	// tenantPlan resolves a tenant's billing plan ("core"/"pro"/
	// "privacy"). It returns ("", nil) when the tenant is unknown so
	// plan-scoped overrides simply don't match rather than erroring.
	tenantPlan(ctx context.Context, tenantID string) (string, error)
}

// resolved is the evaluation-ready form of one flag: the registry
// default plus a scope→enabled lookup keyed by scopeKey.
type resolved struct {
	def       bool
	overrides map[string]bool
}

// scopeKey is the map key used inside resolved.overrides. The global
// scope collapses to a constant key since it has no id.
func scopeKey(scope Scope, id string) string {
	if scope == ScopeGlobal {
		return string(ScopeGlobal)
	}
	return string(scope) + ":" + id
}

// snapshot is an immutable, evaluation-ready view of the whole flag
// set. The Service swaps it atomically under its mutex on refresh.
type snapshot struct {
	flags  map[string]resolved
	loaded time.Time
}

// Service resolves feature flags against a cached snapshot of the
// Postgres-backed store. It is safe for concurrent use.
type Service struct {
	src    source
	logger *log.Logger
	ttl    time.Duration

	mu    sync.RWMutex
	snap  *snapshot
	plans map[string]planEntry // tenantID -> cached plan

	// loadMu serialises snapshot reloads so a burst of concurrent
	// requests that all observe a stale cache triggers a single
	// Postgres load (single-flight) rather than one load per
	// goroutine (a thundering herd against the store). It is held
	// only around the reload, never while serving a fresh snapshot.
	loadMu sync.Mutex

	// planSF gives per-tenant single-flight on cold/stale plan
	// lookups: when many requests for the SAME tenant miss the plan
	// cache at once, only one tenantPlan query runs and the rest
	// share its result. Keyed by tenant id so unrelated tenants still
	// load concurrently (unlike loadMu, which serialises the single
	// global snapshot). This mirrors getSnapshot's single-flight
	// design for the previously-unguarded plan cache.
	planSF singleflight.Group
}

type planEntry struct {
	plan   string
	loaded time.Time
}

// NewStoreService is the production constructor: it wires a Service to
// a pgx-backed [Store]. It performs no I/O (see package doc).
func NewStoreService(store *Store, logger *log.Logger) *Service {
	return newService(store, logger)
}

// newService is the shared constructor used by NewStoreService and by
// tests (which pass a fake source).
func newService(src source, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.Default()
	}
	return &Service{
		src:    src,
		logger: logger,
		ttl:    defaultCacheTTL,
		plans:  make(map[string]planEntry),
	}
}

// WithTTL overrides the snapshot/plan cache TTL. Returns the receiver
// for chaining. A non-positive ttl is ignored.
func (s *Service) WithTTL(ttl time.Duration) *Service {
	if ttl > 0 {
		s.ttl = ttl
	}
	return s
}

// Invalidate drops the cached snapshot and tenant-plan cache so the
// next resolution reloads from Postgres. The admin handlers call this
// after every write so operator changes take effect immediately
// rather than after the TTL.
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.snap = nil
	s.plans = make(map[string]planEntry)
	s.mu.Unlock()
}

// Run refreshes the snapshot every ttl until ctx is cancelled. It is
// registered as a background worker so flag changes made directly in
// Postgres (or by another pod's admin write) propagate within one TTL
// without an explicit Invalidate. It loads once eagerly on entry.
func (s *Service) Run(ctx context.Context) {
	if _, err := s.refresh(ctx); err != nil {
		s.logger.Printf("featureflags: initial refresh: %v", err)
	}
	ticker := time.NewTicker(s.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.refresh(ctx); err != nil {
				s.logger.Printf("featureflags: refresh: %v", err)
			}
		}
	}
}

// refresh loads the full flag set and atomically swaps the snapshot.
func (s *Service) refresh(ctx context.Context) (*snapshot, error) {
	flags, overrides, err := s.src.loadAll(ctx)
	if err != nil {
		return nil, err
	}
	snap := buildSnapshot(flags, overrides)
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	return snap, nil
}

// buildSnapshot folds the registry + overrides into the
// evaluation-ready map form.
func buildSnapshot(flags []Flag, overrides []Override) *snapshot {
	m := make(map[string]resolved, len(flags))
	for _, f := range flags {
		m[f.Key] = resolved{def: f.DefaultEnabled, overrides: map[string]bool{}}
	}
	for _, o := range overrides {
		r, ok := m[o.FlagKey]
		if !ok {
			// Orphan override (flag deleted out from under it). The FK
			// + ON DELETE CASCADE in migration 006 makes this
			// unreachable via the store, but guard anyway so a hand-
			// edited DB can't panic the resolver.
			continue
		}
		r.overrides[scopeKey(o.Scope, o.ScopeID)] = o.Enabled
	}
	return &snapshot{flags: m, loaded: time.Now()}
}

// getSnapshot returns a fresh-enough snapshot, loading lazily if the
// cache is empty or stale. Concurrent callers that find a stale cache
// serialise on loadMu and re-check, so only ONE of them actually
// reloads from Postgres (single-flight); the rest return the snapshot
// that reload produced.
func (s *Service) getSnapshot(ctx context.Context) (*snapshot, error) {
	s.mu.RLock()
	snap := s.snap
	ttl := s.ttl
	s.mu.RUnlock()
	if snap != nil && time.Since(snap.loaded) < ttl {
		return snap, nil
	}

	// Stale/empty: take the reload lock, then re-check — a sibling
	// goroutine may have refreshed while we waited, in which case we
	// skip the redundant store hit.
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.mu.RLock()
	snap = s.snap
	s.mu.RUnlock()
	if snap != nil && time.Since(snap.loaded) < ttl {
		return snap, nil
	}
	return s.refresh(ctx)
}

// EvaluateSubject resolves a flag for an explicit subject. It is the
// pure core of [Service.IsEnabled] and is exported so callers that
// already hold a subject (e.g. background jobs iterating tenants) can
// evaluate without going through request context.
func (s *Service) EvaluateSubject(ctx context.Context, key string, subj Subject) bool {
	snap, err := s.getSnapshot(ctx)
	if err != nil {
		// Fail safe: a store error must not flip a gated feature on.
		s.logger.Printf("featureflags: evaluate %q: snapshot load failed, defaulting off: %v", key, err)
		return false
	}
	r, ok := snap.flags[key]
	if !ok {
		return false // unregistered flag → off
	}
	return evaluate(r, subj)
}

// evaluate applies the precedence ladder against a resolved flag.
func evaluate(r resolved, subj Subject) bool {
	if subj.UserID != "" {
		if v, ok := r.overrides[scopeKey(ScopeUser, subj.UserID)]; ok {
			return v
		}
	}
	if subj.TenantID != "" {
		if v, ok := r.overrides[scopeKey(ScopeTenant, subj.TenantID)]; ok {
			return v
		}
	}
	if subj.Plan != "" {
		if v, ok := r.overrides[scopeKey(ScopePlan, subj.Plan)]; ok {
			return v
		}
	}
	if v, ok := r.overrides[scopeKey(ScopeGlobal, "")]; ok {
		return v
	}
	return r.def
}

// IsEnabled resolves a flag for the caller identified by the request
// context. Tenant and user come from the auth middleware; the tenant's
// plan is resolved (and cached) so plan-scoped rollouts work without
// the caller threading the plan through. A missing tenant context
// simply means tenant/plan overrides cannot match — global + default
// still apply, so unauthenticated/system paths get a sane answer.
func (s *Service) IsEnabled(ctx context.Context, key string) bool {
	subj := Subject{
		TenantID: middleware.TenantIDFrom(ctx),
		UserID:   middleware.KChatUserIDFrom(ctx),
	}
	if subj.TenantID != "" {
		subj.Plan = s.planFor(ctx, subj.TenantID)
	}
	return s.EvaluateSubject(ctx, key, subj)
}

// planFor resolves and caches a tenant's billing plan. A lookup error
// yields "" (no plan-scoped match) rather than failing the evaluation.
//
// Cold/stale lookups for one tenant are de-duplicated via planSF so a
// burst of concurrent requests for that tenant issues a single
// tenantPlan query (per-tenant single-flight) instead of one per
// goroutine.
func (s *Service) planFor(ctx context.Context, tenantID string) string {
	if plan, ok := s.cachedPlan(tenantID); ok {
		return plan
	}

	// Miss/stale: serialise same-tenant loaders on planSF. The leader
	// runs tenantPlan once; followers receive its result without
	// touching Postgres. Re-check the cache inside the closure so a
	// just-finished sibling load short-circuits a redundant query.
	v, err, _ := s.planSF.Do(tenantID, func() (any, error) {
		if plan, ok := s.cachedPlan(tenantID); ok {
			return plan, nil
		}
		plan, err := s.src.tenantPlan(ctx, tenantID)
		if err != nil {
			return "", err
		}
		s.mu.Lock()
		s.plans[tenantID] = planEntry{plan: plan, loaded: time.Now()}
		s.mu.Unlock()
		return plan, nil
	})
	if err != nil {
		s.logger.Printf("featureflags: tenant plan %q: %v", tenantID, err)
		return ""
	}
	return v.(string)
}

// cachedPlan returns the cached plan for tenantID if present and within
// the TTL.
func (s *Service) cachedPlan(tenantID string) (string, bool) {
	s.mu.RLock()
	entry, ok := s.plans[tenantID]
	ttl := s.ttl
	s.mu.RUnlock()
	if ok && time.Since(entry.loaded) < ttl {
		return entry.plan, true
	}
	return "", false
}

// FlagView is one flag plus its overrides, returned by the admin GET.
type FlagView struct {
	Flag
	Overrides []Override `json:"overrides"`
}

// List returns every registered flag with its overrides, sorted by key
// (and overrides sorted by scope then id) for stable admin output.
func (s *Service) List(ctx context.Context) ([]FlagView, error) {
	flags, overrides, err := s.src.loadAll(ctx)
	if err != nil {
		return nil, err
	}
	return assembleViews(flags, overrides), nil
}

// assembleViews folds a flat flag list + override list into the nested
// admin view, sorted deterministically. Shared by Service.List and
// Store.loadViews so the two stay byte-identical.
func assembleViews(flags []Flag, overrides []Override) []FlagView {
	views := make([]FlagView, 0, len(flags))
	for _, f := range flags {
		views = append(views, FlagView{Flag: f, Overrides: []Override{}})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Key < views[j].Key })
	byKey := make(map[string]*FlagView, len(views))
	for i := range views {
		byKey[views[i].Key] = &views[i]
	}
	for _, o := range overrides {
		if v, ok := byKey[o.FlagKey]; ok {
			v.Overrides = append(v.Overrides, o)
		}
	}
	for i := range views {
		ovs := views[i].Overrides
		sort.Slice(ovs, func(a, b int) bool {
			if ovs[a].Scope != ovs[b].Scope {
				return scopePrecedence[ovs[a].Scope] < scopePrecedence[ovs[b].Scope]
			}
			return ovs[a].ScopeID < ovs[b].ScopeID
		})
	}
	return views
}

// scopePrecedence gives a stable sort order (least→most specific) for
// admin output. It does NOT drive evaluation — evaluate() walks the
// ladder explicitly — but keeping the two in the same file makes the
// ordering intent obvious.
var scopePrecedence = map[Scope]int{
	ScopeGlobal: 0,
	ScopePlan:   1,
	ScopeTenant: 2,
	ScopeUser:   3,
}
