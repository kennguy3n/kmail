package search

import (
	"context"
	"errors"
	"fmt"
	"log"
	mathrand "math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CutoverTransition pairs a `source` backend with the `target`
// backend a tenant on `source` should be promoted to. The worker
// runs one full scan per configured transition each tick. Two
// transitions ship by default:
//
//   - {meilisearch -> opensearch}: the legacy per-tenant index
//     path that existed before the shared-index work landed.
//   - {shared_meilisearch -> shared_opensearch}: the modern
//     shared-index path for tenants on the shared-index default.
//
// Operators who need a custom pair (e.g. forcing a shared tenant
// onto a dedicated index) inject their own slice via
// `CutoverConfig.Transitions`.
type CutoverTransition struct {
	// Source is the `tenants.search_backend` value that makes a
	// tenant eligible. The worker filters ListCandidates on it.
	Source string
	// Target is the backend the worker reindexes INTO and then
	// SetBackend flips the column to. The worker only marks a
	// row `completed` after Target is fully populated.
	Target string
}

// DefaultCutoverTransitions is what the worker uses when no
// Transitions are explicitly configured. Listed in execution
// order — the legacy pair runs first because that's the path
// that has historically been in production.
var DefaultCutoverTransitions = []CutoverTransition{
	{Source: BackendMeilisearch, Target: BackendOpenSearch},
	{Source: BackendSharedMeilisearch, Target: BackendSharedOpenSearch},
}

// CutoverConfig parameterises the auto-cutover worker.
//
//	Threshold:        mailbox-size (bytes) at or above which a tenant
//	                  becomes a candidate. Defaults to the per-tenant
//	                  equivalent of 100k messages × ~16 KiB / message.
//	Interval:         how often the worker scans for eligible tenants.
//	                  Defaults to 5m. The worker also runs once at start.
//	MaxFailures:      after this many consecutive Reindex failures,
//	                  the worker leaves the tenant in `failed` state
//	                  and stops retrying. Defaults to 5.
//	MaxRetryGap:      the worker won't retry a `failed` job until this
//	                  long after its last `updated_at`. Defaults to 1h.
//	                  Mostly relevant when a transient OpenSearch
//	                  outage causes a wave of failures the worker
//	                  shouldn't bash on every tick.
//	ReconcileAfter:   how long an `in_progress` row whose tenant is
//	                  already on OpenSearch is allowed to sit before
//	                  the worker forcibly promotes it to `completed`.
//	                  Defaults to 30m. The migration normally finishes
//	                  within seconds-to-minutes, so 30m is well past
//	                  any legitimate in-flight migration window.
//	Concurrency:      maximum number of tenants migrated in parallel
//	                  within a single transition's scan. Defaults to 2
//	                  so a tick can make progress on multiple hot
//	                  tenants without saturating the destination
//	                  backend's bulk-import path.
type CutoverConfig struct {
	Pool    *pgxpool.Pool
	Store   CutoverStore
	Service BackendFlipper
	Sizer   MailboxSizer
	Source  MessageSource
	Logger  *log.Logger
	// Audit, when non-nil, receives a `system`-actor entry for
	// every completed / failed cutover so the migration is
	// visible in the tamper-evident audit trail. Nil disables
	// audit logging (the worker still functions).
	Audit AuditLogger
	// Metrics is the Prometheus metric set the worker increments
	// on each completed / failed cutover and the in-flight gauge.
	// Nil is replaced with an unregistered set (counts still
	// happen, they're just not exported) so increments are always
	// safe.
	Metrics        *CutoverMetrics
	Threshold      int64
	Interval       time.Duration
	MaxFailures    int
	MaxRetryGap    time.Duration
	ReconcileAfter time.Duration
	// Concurrency caps how many tenants the worker migrates in
	// parallel per transition. Defaults to 2. Values <= 0 fall
	// back to the default.
	Concurrency int
	// Transitions enumerates the (source, target) backend pairs
	// the worker considers each tick. Defaults to
	// `DefaultCutoverTransitions` (legacy meili->opensearch plus
	// modern shared_meili->shared_opensearch). Operators can
	// override to bias only one path or to add a custom pair.
	Transitions []CutoverTransition
	// MarkCompletedRetries bounds the retry loop around the
	// post-SetBackend MarkCompleted call. Defaults to 3. A bounded
	// retry covers a Postgres connection blip without spinning the
	// goroutine on a genuinely-broken store; the periodic
	// reconciliation pass picks up anything the retry can't.
	MarkCompletedRetries int
	// StartupJitter caps the random delay applied before the first
	// Tick when `Run` is invoked. Spreads the deploy-time burst of
	// ListCandidates / TenantMailboxSize calls across pods so a
	// rolling restart of N replicas doesn't hammer the database +
	// Stalwart admin API in a single instant. Defaults to 30s; set
	// to 0 for deterministic test runs that drive `Run` directly.
	StartupJitter time.Duration
	// Now is the wall-clock source; defaults to time.Now. Tests
	// inject a fixed clock so retry-backoff is deterministic.
	Now func() time.Time
	// Sleep lets tests skip the retry backoff. Defaults to
	// time.Sleep. In production this only ever fires on the rare
	// post-SetBackend MarkCompleted retry path, so the latency
	// hit (a few hundred ms) is acceptable.
	Sleep func(time.Duration)

	// disableStartupJitter is set internally by the test entry
	// point so the default-applied jitter doesn't slow the unit
	// suite. Not exported — production should always go through
	// the explicit `StartupJitter` knob or accept the default.
	disableStartupJitter bool
}

// DisableStartupJitter is the explicit opt-out used by tests that
// drive `Run` directly. Production callers should set
// `CutoverConfig.StartupJitter` to a positive value (or leave it
// zero to accept the default) instead.
func DisableStartupJitter(cfg *CutoverConfig) {
	cfg.disableStartupJitter = true
}

// CutoverWorker auto-promotes tenants from Meilisearch to
// OpenSearch when their mailbox size crosses the configured
// threshold. Run it once per pod via `Run(ctx)` — the worker
// claims tenants atomically through the store so two replicas can
// race the same tick without double-migrating the same tenant.
type CutoverWorker struct {
	cfg  CutoverConfig
	exec migrationDeps
}

const (
	defaultCutoverThreshold          = 100_000 * 16 * 1024 // ~100k messages × 16 KiB
	defaultCutoverInterval           = 5 * time.Minute
	defaultCutoverMaxFailures        = 5
	defaultCutoverMaxRetryGap        = time.Hour
	defaultCutoverReconcileAfter     = 30 * time.Minute
	defaultCutoverMarkCompletedTries = 3
	defaultCutoverStartupJitter      = 30 * time.Second
	defaultCutoverConcurrency        = 2
	cutoverMarkCompletedBaseBackoff  = 250 * time.Millisecond
)

// NewCutoverWorker wires the worker. Validates required deps so a
// misconfiguration surfaces at startup, not on the first tick.
func NewCutoverWorker(cfg CutoverConfig) (*CutoverWorker, error) {
	if cfg.Service == nil {
		return nil, errors.New("search.NewCutoverWorker: Service is required")
	}
	if cfg.Sizer == nil {
		return nil, errors.New("search.NewCutoverWorker: Sizer is required")
	}
	if cfg.Source == nil {
		return nil, errors.New("search.NewCutoverWorker: Source is required")
	}
	if cfg.Store == nil {
		if cfg.Pool == nil {
			return nil, errors.New("search.NewCutoverWorker: Store or Pool is required")
		}
		cfg.Store = NewPostgresCutoverStore(cfg.Pool)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultCutoverThreshold
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultCutoverInterval
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = defaultCutoverMaxFailures
	}
	if cfg.MaxRetryGap <= 0 {
		cfg.MaxRetryGap = defaultCutoverMaxRetryGap
	}
	if cfg.ReconcileAfter <= 0 {
		cfg.ReconcileAfter = defaultCutoverReconcileAfter
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultCutoverConcurrency
	}
	if len(cfg.Transitions) == 0 {
		cfg.Transitions = DefaultCutoverTransitions
	}
	if cfg.MarkCompletedRetries <= 0 {
		cfg.MarkCompletedRetries = defaultCutoverMarkCompletedTries
	}
	if cfg.StartupJitter < 0 {
		// Negative is operator-error; treat as zero (no jitter)
		// rather than panicking inside mathrand.Int64N.
		cfg.StartupJitter = 0
	} else if cfg.StartupJitter == 0 && !cfg.disableStartupJitter {
		cfg.StartupJitter = defaultCutoverStartupJitter
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NewCutoverMetrics(nil)
	}
	w := &CutoverWorker{
		cfg: cfg,
		exec: migrationDeps{
			store:                cfg.Store,
			flipper:              cfg.Service,
			source:               cfg.Source,
			audit:                cfg.Audit,
			metrics:              cfg.Metrics,
			logger:               cfg.Logger,
			now:                  cfg.Now,
			sleep:                cfg.Sleep,
			markCompletedRetries: cfg.MarkCompletedRetries,
		},
	}
	return w, nil
}

// Run drives the worker in a loop until ctx is cancelled. Each
// tick is independent — a failing tick logs and waits out the
// interval rather than terminating the goroutine.
//
// A small randomised delay (0..cfg.StartupJitter) precedes the
// initial tick so a rolling deployment with N replicas doesn't
// fire N simultaneous ListCandidates + N×M TenantMailboxSize
// calls within the same millisecond. The jitter is bounded so the
// "don't wait a full interval after restart" guarantee still
// holds. Tests that exercise `Run` can set StartupJitter to 0 to
// keep behavior deterministic; tests that drive the state machine
// directly via `Tick` are unaffected.
func (w *CutoverWorker) Run(ctx context.Context) {
	if w.cfg.StartupJitter > 0 {
		jitter := time.Duration(mathrand.Int64N(int64(w.cfg.StartupJitter)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter):
		}
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	// One immediate tick so a pod restart doesn't add up to a
	// full interval of delay on tenants who are already past
	// the threshold.
	w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick scans for eligible tenants and migrates each one. Exposed
// (capitalised) so tests can drive the worker deterministically
// without spinning the time.Ticker. Errors per tenant are logged
// and counted; they do NOT abort the tick (one bad tenant must not
// stop the rest of the fleet from migrating).
//
// The tick walks every configured transition pair in order. For
// each pair it:
//
//  1. Runs the reconcile passes scoped to that pair's source /
//     target so a stale `in_progress` row left by a previous
//     transition's worker is cleaned up before new candidates
//     are scanned.
//  2. Lists candidates whose `search_backend` matches the
//     pair's source AND for which there is no existing
//     completed-or-blocking job row for the pair's target.
//  3. Migrates each candidate from source -> target.
//
// `search_cutover_jobs` rows are keyed by
// `(tenant_id, target_backend)`, so transitions are first-class:
// a tenant previously promoted to one target can re-enter the
// pipeline against a different target without a manual row
// reset. Every store method below threads the target backend
// through so two concurrent transitions on the same tenant don't
// collide.
func (w *CutoverWorker) Tick(ctx context.Context) {
	for _, tr := range w.cfg.Transitions {
		w.tickTransition(ctx, tr)
	}
}

// tickTransition runs one (source, target) pair of the cutover
// pipeline. Two complementary reconcile passes prefix the scan:
//
//	(a) ReconcileCompleted: tenant ALREADY on `target`
//	    (SetBackend committed, MarkCompleted didn't). Promote
//	    the row to `completed`.
//	(b) ReconcileStale:     tenant STILL on `source`
//	    (pod crashed during ReindexTo or before SetBackend).
//	    Demote the row to `failed` so the normal back-off /
//	    retry path picks it up on a subsequent tick. Without
//	    this, the row sits in `in_progress` forever — neither
//	    ListCandidates (excludes `in_progress`) nor
//	    ReconcileCompleted (gates on `search_backend = target`)
//	    recovers it.
//
// Reconciliation failures are logged and ignored — the main
// cutover loop is independent of them, and a transient store
// blip shouldn't bring the worker down.
//
// Eligible candidates are migrated in parallel, bounded by
// `cfg.Concurrency`, so a single tick makes progress on multiple
// hot tenants without launching an unbounded goroutine fan-out
// against the destination backend.
func (w *CutoverWorker) tickTransition(ctx context.Context, tr CutoverTransition) {
	now := w.cfg.Now()
	staleBefore := now.Add(-w.cfg.ReconcileAfter)
	if n, err := w.cfg.Store.ReconcileCompleted(ctx, tr.Target, staleBefore, now); err != nil {
		w.cfg.Logger.Printf("search.cutover[%s->%s]: reconcile completed: %v", tr.Source, tr.Target, err)
	} else if n > 0 {
		w.cfg.Logger.Printf("search.cutover[%s->%s]: reconcile completed: promoted %d stale in_progress rows to completed", tr.Source, tr.Target, n)
	}
	if n, err := w.cfg.Store.ReconcileStale(ctx, tr.Source, tr.Target, staleBefore, now); err != nil {
		w.cfg.Logger.Printf("search.cutover[%s->%s]: reconcile stale: %v", tr.Source, tr.Target, err)
	} else if n > 0 {
		w.cfg.Logger.Printf("search.cutover[%s->%s]: reconcile stale: demoted %d crashed in_progress rows to failed", tr.Source, tr.Target, n)
	}
	ids, err := w.cfg.Store.ListCandidates(ctx, CandidateFilter{
		SourceBackend:    tr.Source,
		TargetBackend:    tr.Target,
		MaxFailures:      w.cfg.MaxFailures,
		RetryAfterBefore: now.Add(-w.cfg.MaxRetryGap),
	})
	if err != nil {
		w.cfg.Logger.Printf("search.cutover[%s->%s]: list candidates: %v", tr.Source, tr.Target, err)
		return
	}
	// Bounded fan-out: at most cfg.Concurrency cutovers run at
	// once. `sem` is the token bucket; `wg` lets the tick block
	// until every spawned migration has finished so reconcile
	// windows and the next tick see a settled state.
	sem := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, tenantID := range ids {
		size, err := w.cfg.Sizer.TenantMailboxSize(ctx, tenantID)
		if err != nil {
			w.cfg.Logger.Printf("search.cutover[%s->%s]: sizer tenant=%s: %v", tr.Source, tr.Target, tenantID, err)
			continue
		}
		if size < w.cfg.Threshold {
			continue
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(tenantID string, size int64) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := w.cutoverOne(ctx, tenantID, size, tr); err != nil {
				w.cfg.Logger.Printf("search.cutover[%s->%s]: tenant %s: %v", tr.Source, tr.Target, tenantID, err)
			}
		}(tenantID, size)
	}
	wg.Wait()
}

// cutoverOne claims a tenant's (tenant, target) job row and, if
// the claim wins, drives the full migration dance via
// runMigration. Concurrent workers race the `Claim` call; the
// loser short-circuits and lets the winner finish.
//
// The `tr` parameter scopes which (source, target) backend pair
// the migration is for — the same worker handles every
// configured pair in turn, so the destination is not implicit.
func (w *CutoverWorker) cutoverOne(ctx context.Context, tenantID string, size int64, tr CutoverTransition) error {
	now := w.cfg.Now()
	claimed, err := w.cfg.Store.Claim(ctx, tenantID, tr.Target, size, w.cfg.Threshold, now)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if !claimed {
		// Another worker grabbed it; let them finish.
		return nil
	}
	return runMigration(ctx, w.exec, tenantID, tr.Source, tr.Target)
}
