package search

import (
	"context"
	"errors"
	"fmt"
	"log"
	mathrand "math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MailboxSizer reports the cumulative mailbox size for a tenant in
// bytes. The auto-cutover worker uses this to decide whether the
// tenant has outgrown the Meilisearch single-node ceiling. Production
// wires this against Stalwart's admin API (`/admin/tenant/{id}/storage`);
// tests inject a fake. The contract is intentionally minimal — the
// worker doesn't care HOW the size is computed, only what it is.
type MailboxSizer interface {
	TenantMailboxSize(ctx context.Context, tenantID string) (int64, error)
}

// MailboxSizerFunc adapts a closure into the MailboxSizer
// interface, useful for inline wiring in main.go and for tests.
type MailboxSizerFunc func(ctx context.Context, tenantID string) (int64, error)

// TenantMailboxSize implements MailboxSizer.
func (f MailboxSizerFunc) TenantMailboxSize(ctx context.Context, tenantID string) (int64, error) {
	return f(ctx, tenantID)
}

// MessageSource pulls every indexable message for a tenant so the
// cutover worker can hand them to `Service.Reindex`. Production
// wires this against Stalwart's JMAP `Email/query` + `Email/get`
// loop; tests return a slice directly. Returning the full slice
// (rather than a stream) is fine for Phase 5 since the cutover
// threshold is bounded (default ~100k messages); a streaming
// refinement is a future optimisation.
type MessageSource interface {
	MessagesForTenant(ctx context.Context, tenantID string) ([]Message, error)
}

// MessageSourceFunc adapts a closure into the MessageSource
// interface for inline wiring.
type MessageSourceFunc func(ctx context.Context, tenantID string) ([]Message, error)

// MessagesForTenant implements MessageSource.
func (f MessageSourceFunc) MessagesForTenant(ctx context.Context, tenantID string) ([]Message, error) {
	return f(ctx, tenantID)
}

// BackendFlipper is the subset of *Service the cutover worker
// needs. Order matters: the worker MUST reindex into the target
// backend first (via ReindexTo, which targets a specific backend
// regardless of the tenant's current `search_backend` column) and
// only flip the column via SetBackend after the reindex succeeds.
// If we flipped the column first and the reindex failed, the
// tenant would be permanently invisible to retries (the candidate
// query filters on `search_backend = 'meilisearch'`) and stuck
// reading from an empty OpenSearch index.
type BackendFlipper interface {
	// ReindexTo bulk-imports `msgs` into a SPECIFIC backend,
	// independent of the tenant's `search_backend` column. The
	// worker calls this BEFORE SetBackend so a transient
	// destination-side failure (OpenSearch 502, network blip)
	// leaves the tenant readable on the source backend.
	ReindexTo(ctx context.Context, tenantID, backend string, msgs []Message) error
	// SetBackend flips the tenant's `search_backend` column.
	// Called only AFTER ReindexTo succeeds so reads route to a
	// fully-populated index.
	SetBackend(ctx context.Context, tenantID, backend string) error
}

// CutoverState enumerates the per-tenant cutover job states.
type CutoverState string

const (
	CutoverPending    CutoverState = "pending"
	CutoverInProgress CutoverState = "in_progress"
	CutoverCompleted  CutoverState = "completed"
	CutoverFailed     CutoverState = "failed"
)

// CandidateFilter parameterises CutoverStore.ListCandidates so the
// store can do the eligibility join in SQL (default impl) or in
// memory (test impl) without the worker reaching into store
// internals.
type CandidateFilter struct {
	// SourceBackend is the backend a tenant must currently be on
	// to be considered eligible. Together with TargetBackend it
	// uniquely identifies a transition.
	SourceBackend string
	// TargetBackend is the backend the worker intends to promote
	// the tenant TO. The store keys job rows by
	// `(tenant_id, target_backend)` (migration 051) so a tenant
	// previously promoted to a different target is NOT shielded
	// by an old `completed` row when a new transition needs to
	// run — e.g. an operator manually reverts a tenant from
	// `shared_opensearch` to `shared_meilisearch` and the worker
	// must be able to re-promote it. Required.
	TargetBackend string
	// MaxFailures excludes tenants whose `failure_count` has
	// reached this value.
	MaxFailures int
	// RetryAfterBefore excludes `failed` rows whose `updated_at`
	// is more recent than this timestamp, implementing the
	// failure back-off window.
	RetryAfterBefore time.Time
}

// CutoverStore is the storage surface the worker depends on. The
// default Postgres-backed impl is `NewPostgresCutoverStore`; the
// `inMemoryCutoverStore` in `cutover_test.go` is what tests use.
// Decoupling the worker from pgx lets us exercise every state
// transition deterministically without a real database.
type CutoverStore interface {
	// ListCandidates returns tenant IDs that are eligible for a
	// cutover attempt under `f`. The store is responsible for
	// excluding rows that are already `in_progress` or `completed`
	// FOR THE SAME (tenant, target_backend) PAIR. A tenant
	// previously completed against a different target is still a
	// candidate for `f.TargetBackend`.
	ListCandidates(ctx context.Context, f CandidateFilter) ([]string, error)
	// Claim transitions a tenant's job row to `in_progress` if no
	// other worker is currently holding it. Must be atomic — the
	// caller relies on a single winner across replicas.
	// `targetBackend` selects which (tenant, target) row to claim
	// so two transitions on the same tenant don't collide.
	Claim(ctx context.Context, tenantID, targetBackend string, size, threshold int64, now time.Time) (bool, error)
	// MarkCompleted moves the row to `completed` and resets the
	// failure counter for the given (tenant, target).
	MarkCompleted(ctx context.Context, tenantID, targetBackend string, now time.Time) error
	// MarkFailed moves the row to `failed`, increments
	// `failure_count`, and records `reason`, scoped to the given
	// (tenant, target).
	MarkFailed(ctx context.Context, tenantID, targetBackend, reason string, now time.Time) error
	// ReconcileCompleted promotes any `in_progress` row to
	// `completed` when the tenant has ALREADY been flipped to
	// `targetBackend` (i.e., `Service.SetBackend` succeeded) AND
	// the row hasn't been updated since `before`. This is the
	// safety net for the unlucky case where SetBackend committed
	// but the follow-up MarkCompleted failed (Postgres blip, pod
	// killed mid-write). Without this, those rows would sit in
	// `in_progress` forever — they're not retryable via
	// ListCandidates (the tenant is already on OpenSearch) and
	// the bookkeeping eventually skews the cutover dashboard.
	// Idempotent: rows already in another state are left alone.
	// Returns the count of rows promoted so the worker can log it.
	// `now` is the timestamp written to `completed_at` / `updated_at`
	// — the worker passes `cfg.Now()` so the same injectable clock
	// used by Claim/MarkCompleted/MarkFailed governs this path too,
	// keeping integration tests deterministic.
	ReconcileCompleted(ctx context.Context, targetBackend string, before, now time.Time) (int64, error)
	// ReconcileStale demotes `in_progress` rows BACK to `failed`
	// when the tenant is still on `sourceBackend` (i.e., SetBackend
	// has NOT been called yet) AND the row hasn't been updated
	// since `before`. This is the complementary safety net to
	// ReconcileCompleted: it handles a pod that crashed AFTER
	// Claim but BEFORE either SetBackend or MarkFailed
	// (OOM-kill / SIGKILL / node failure during ReindexTo).
	// Without this, those rows would sit in `in_progress` forever:
	//
	//   - ListCandidates excludes `in_progress` rows (`cutover_state
	//     <> 'in_progress'`), so the next tick can't pick them up.
	//   - ReconcileCompleted only handles tenants ALREADY on the
	//     target backend.
	//
	// `targetBackend` scopes the demotion to the (tenant, target)
	// row owned by the current transition so a parallel transition
	// for the same tenant under a different target isn't touched.
	//
	// Demotion to `failed` (with an incremented `failure_count` and
	// a synthetic reason) lets the normal retry pathway re-promote
	// the row through ListCandidates on the next tick, governed by
	// the same `MaxFailures` / `MaxRetryGap` back-off as ordinary
	// failures so a wedged tenant doesn't pin a worker forever.
	// Idempotent: rows already in another state are left alone.
	// Returns the count of rows demoted so the worker can log it.
	// `now` is written to `updated_at` / `failed_at`; the worker
	// passes `cfg.Now()` so all four reconciliation/claim/mark
	// methods share one clock source.
	ReconcileStale(ctx context.Context, sourceBackend, targetBackend string, before, now time.Time) (int64, error)
}

// CutoverTransition pairs a `source` backend with the `target`
// backend a tenant on `source` should be promoted to. The worker
// runs one full scan per configured transition each tick. Two
// transitions ship by default:
//
//   - {meilisearch -> opensearch}: the legacy per-tenant index
//     path that existed before the shared-index work landed.
//   - {shared_meilisearch -> shared_opensearch}: the modern
//     shared-index path for tenants on the migration-050 default.
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
//	                  Defaults to 1h. The worker also runs once at start.
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
type CutoverConfig struct {
	Pool           *pgxpool.Pool
	Store          CutoverStore
	Service        BackendFlipper
	Sizer          MailboxSizer
	Source         MessageSource
	Logger         *log.Logger
	Threshold      int64
	Interval       time.Duration
	MaxFailures    int
	MaxRetryGap    time.Duration
	ReconcileAfter time.Duration
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
	cfg CutoverConfig
}

const (
	defaultCutoverThreshold          = 100_000 * 16 * 1024 // ~100k messages × 16 KiB
	defaultCutoverInterval           = time.Hour
	defaultCutoverMaxFailures        = 5
	defaultCutoverMaxRetryGap        = time.Hour
	defaultCutoverReconcileAfter     = 30 * time.Minute
	defaultCutoverMarkCompletedTries = 3
	defaultCutoverStartupJitter      = 30 * time.Second
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
	return &CutoverWorker{cfg: cfg}, nil
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
// Migration 051 keys `search_cutover_jobs` rows by
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
//   (a) ReconcileCompleted: tenant ALREADY on `target`
//       (SetBackend committed, MarkCompleted didn't). Promote
//       the row to `completed`.
//   (b) ReconcileStale:     tenant STILL on `source`
//       (pod crashed during ReindexTo or before SetBackend).
//       Demote the row to `failed` so the normal back-off /
//       retry path picks it up on a subsequent tick. Without
//       this, the row sits in `in_progress` forever — neither
//       ListCandidates (excludes `in_progress`) nor
//       ReconcileCompleted (gates on `search_backend = target`)
//       recovers it.
//
// Reconciliation failures are logged and ignored — the main
// cutover loop is independent of them, and a transient store
// blip shouldn't bring the worker down.
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
	for _, tenantID := range ids {
		size, err := w.cfg.Sizer.TenantMailboxSize(ctx, tenantID)
		if err != nil {
			w.cfg.Logger.Printf("search.cutover[%s->%s]: sizer tenant=%s: %v", tr.Source, tr.Target, tenantID, err)
			continue
		}
		if size < w.cfg.Threshold {
			continue
		}
		if err := w.cutoverOne(ctx, tenantID, size, tr); err != nil {
			w.cfg.Logger.Printf("search.cutover[%s->%s]: tenant %s: %v", tr.Source, tr.Target, tenantID, err)
		}
	}
}

// cutoverOne runs the full cutover dance for a single tenant. The
// state machine is the load-bearing piece: every state transition
// is a separate store write so a crash mid-flight resumes from the
// last persisted state. Concurrent workers race the `Claim` call;
// the loser short-circuits and lets the winner finish.
//
// The `tr` parameter scopes which (source, target) backend pair
// the migration is for — the same worker handles every
// configured pair in turn, so the destination is not implicit.
// Every store call threads `tr.Target` through so the claim, mark,
// and reconcile paths all key on `(tenant_id, target_backend)`
// per migration 051.
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
	// From here on, every error path MUST flip the row to
	// `failed` so the next tick can retry.
	msgs, fetchErr := w.cfg.Source.MessagesForTenant(ctx, tenantID)
	if fetchErr != nil {
		_ = w.cfg.Store.MarkFailed(ctx, tenantID, tr.Target, fmt.Sprintf("fetch messages: %v", fetchErr), w.cfg.Now())
		return fmt.Errorf("fetch messages: %w", fetchErr)
	}
	// Reindex into `tr.Target` FIRST so reads keep going to
	// `tr.Source` until the target is fully populated. If the
	// reindex fails for any reason — destination 502, partial
	// network failure, schema rejection — the tenant's
	// `search_backend` column is still the source, which keeps
	// the tenant readable AND keeps it visible to
	// `ListCandidates` for the next retry. ReindexTo deletes the
	// destination index first so a half-written previous attempt
	// doesn't leave orphan documents.
	if err := w.cfg.Service.ReindexTo(ctx, tenantID, tr.Target, msgs); err != nil {
		_ = w.cfg.Store.MarkFailed(ctx, tenantID, tr.Target, fmt.Sprintf("reindex: %v", err), w.cfg.Now())
		return fmt.Errorf("reindex: %w", err)
	}
	// Target is now warm; atomically flip reads over. A SetBackend
	// failure here is the unfortunate-but-recoverable case: the
	// target index is fully populated but reads still go to the
	// source. The next tick re-discovers the tenant (still on the
	// source), the ReindexTo wipes & re-fills the target
	// (idempotent), and the SetBackend is retried.
	if err := w.cfg.Service.SetBackend(ctx, tenantID, tr.Target); err != nil {
		_ = w.cfg.Store.MarkFailed(ctx, tenantID, tr.Target, fmt.Sprintf("set backend: %v", err), w.cfg.Now())
		return fmt.Errorf("set backend: %w", err)
	}
	// SetBackend committed; the tenant is live on `tr.Target`.
	// MarkCompleted is the bookkeeping update — it must not
	// cause a re-migration if it transiently fails. Retry with
	// exponential backoff to absorb a single connection blip; if
	// the retry budget is exhausted, the next Tick's
	// ReconcileCompleted pass will promote the row (the tenant
	// is already on `tr.Target`, so the reconcile guard fires).
	if err := w.markCompletedWithRetry(ctx, tenantID, tr.Target); err != nil {
		w.cfg.Logger.Printf("search.cutover[%s->%s]: tenant=%s SetBackend OK but MarkCompleted persistently failed; will be reconciled on next tick: %v", tr.Source, tr.Target, tenantID, err)
		return fmt.Errorf("mark completed: %w", err)
	}
	w.cfg.Logger.Printf("search.cutover[%s->%s]: tenant=%s migrated %d messages", tr.Source, tr.Target, tenantID, len(msgs))
	return nil
}

// markCompletedWithRetry retries MarkCompleted with exponential
// backoff up to cfg.MarkCompletedRetries times. Ctx-cancellation
// short-circuits the loop immediately so a pod shutdown isn't
// delayed by the backoff.
func (w *CutoverWorker) markCompletedWithRetry(ctx context.Context, tenantID, targetBackend string) error {
	var lastErr error
	for attempt := 0; attempt < w.cfg.MarkCompletedRetries; attempt++ {
		if err := w.cfg.Store.MarkCompleted(ctx, tenantID, targetBackend, w.cfg.Now()); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < w.cfg.MarkCompletedRetries-1 {
			backoff := cutoverMarkCompletedBaseBackoff << attempt
			w.cfg.Sleep(backoff)
		}
	}
	return lastErr
}

// PostgresCutoverStore is the default CutoverStore wired against
// Postgres. The schema lives in migration 046.
type PostgresCutoverStore struct {
	pool *pgxpool.Pool
}

// NewPostgresCutoverStore wraps a pool.
func NewPostgresCutoverStore(pool *pgxpool.Pool) *PostgresCutoverStore {
	return &PostgresCutoverStore{pool: pool}
}

// ListCandidates implements CutoverStore. The LEFT JOIN is scoped
// to the SAME `(tenant_id, target_backend)` as the candidate
// transition: a tenant with a `completed` row for a DIFFERENT
// target is still eligible. Filter SQL:
//
//   - tenant currently on f.SourceBackend, AND
//   - no job row for f.TargetBackend, OR the only row for
//     f.TargetBackend is a recoverable `failed` (failure_count
//     and back-off window predicates apply).
//
// `in_progress` rows are NOT candidates — they're either being
// actively driven by another worker (the Claim race-loser path)
// or about to be reconciled by ReconcileStale on a future tick.
func (s *PostgresCutoverStore) ListCandidates(ctx context.Context, f CandidateFilter) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text
		FROM tenants t
		LEFT JOIN search_cutover_jobs j
		       ON j.tenant_id      = t.id
		      AND j.target_backend = $2
		WHERE t.search_backend = $1
		  AND (
		      j.tenant_id IS NULL
		      OR (j.cutover_state = 'failed'
		          AND j.failure_count < $3
		          AND j.updated_at < $4)
		  )
	`, f.SourceBackend, f.TargetBackend, f.MaxFailures, f.RetryAfterBefore)
	if err != nil {
		return nil, fmt.Errorf("scan tenants: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// Claim implements CutoverStore. The UPSERT-then-conditional-UPDATE
// dance runs in one transaction so two replicas racing the same
// (tenant, target) pair land on a single winner. The composite
// `(tenant_id, target_backend)` PK lets the same tenant carry
// multiple rows (one per target) simultaneously.
func (s *PostgresCutoverStore) Claim(ctx context.Context, tenantID, targetBackend string, size, threshold int64, now time.Time) (bool, error) {
	var claimed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO search_cutover_jobs (tenant_id, target_backend, cutover_state, mailbox_size, threshold, started_at, updated_at)
			VALUES ($1, $2, 'pending', $3, $4, NULL, $5)
			ON CONFLICT (tenant_id, target_backend) DO NOTHING
		`, tenantID, targetBackend, size, threshold, now)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE search_cutover_jobs
			   SET cutover_state = 'in_progress',
			       mailbox_size  = $3,
			       started_at    = $4,
			       updated_at    = $4
			 WHERE tenant_id      = $1
			   AND target_backend = $2
			   AND cutover_state IN ('pending', 'failed')
		`, tenantID, targetBackend, size, now)
		if err != nil {
			return err
		}
		claimed = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// MarkCompleted implements CutoverStore. Scoped to the specific
// (tenant, target) row so a parallel transition on the same
// tenant isn't accidentally finalised.
func (s *PostgresCutoverStore) MarkCompleted(ctx context.Context, tenantID, targetBackend string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs
		   SET cutover_state = 'completed',
		       completed_at  = $3,
		       updated_at    = $3,
		       failure_count = 0,
		       last_error    = ''
		 WHERE tenant_id      = $1
		   AND target_backend = $2
	`, tenantID, targetBackend, now)
	return err
}

// MarkFailed implements CutoverStore. Scoped to the specific
// (tenant, target) row so two transitions sharing a tenant don't
// pollute each other's failure_count / last_error.
func (s *PostgresCutoverStore) MarkFailed(ctx context.Context, tenantID, targetBackend, reason string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs
		   SET cutover_state = 'failed',
		       failure_count = failure_count + 1,
		       last_error    = $3,
		       updated_at    = $4
		 WHERE tenant_id      = $1
		   AND target_backend = $2
	`, tenantID, targetBackend, reason, now)
	return err
}

// ReconcileCompleted implements CutoverStore. The UPDATE joins
// against `tenants.search_backend` so the promotion is gated on
// the actual production state: only rows whose tenant is ALREADY
// on `targetBackend` AND whose job row keys to the same target
// get promoted. The `target_backend` scope prevents a parallel
// transition's `in_progress` row (different target) from being
// accidentally promoted because the tenant happens to be on this
// reconcile's target. `updated_at` is bumped to `now`
// (caller-provided via `cfg.Now()`) so the dashboard reflects the
// recovery and integration tests can drive a deterministic clock.
func (s *PostgresCutoverStore) ReconcileCompleted(ctx context.Context, targetBackend string, before, now time.Time) (int64, error) {
	// Both `j.target_backend = $1` and `t.search_backend = $1`
	// intentionally compare the same `$1` parameter: the row is
	// only a reconcile candidate when the JOB targeted this
	// backend AND the tenant has already been flipped to this
	// backend (SetBackend committed but MarkCompleted didn't). A
	// mismatched pair is either the wrong transition or the
	// stuck state ReconcileStale handles.
	tag, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs j
		   SET cutover_state = 'completed',
		       completed_at  = $3,
		       updated_at    = $3,
		       failure_count = 0,
		       last_error    = ''
		  FROM tenants t
		 WHERE j.tenant_id       = t.id
		   AND j.target_backend  = $1
		   AND j.cutover_state   = 'in_progress'
		   AND j.updated_at      < $2
		   AND t.search_backend  = $1
	`, targetBackend, before, now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ReconcileStale implements CutoverStore. Mirror of
// ReconcileCompleted for the opposite outcome: when a pod crashes
// between Claim and SetBackend, the row sits in `in_progress` with
// the tenant still on `sourceBackend`. The UPDATE demotes those
// rows back to `failed` (incrementing `failure_count` and stamping
// a synthetic reason) so the normal retry pathway can pick them up
// on the next tick.
//
// Predicates:
//   - `updated_at < $2` gates by the reconciliation horizon so
//     an in-flight migration on another worker isn't incorrectly
//     demoted.
//   - `tenants.search_backend = $1` guarantees we only touch
//     tenants still on the source backend — a tenant already on
//     the target would be handled by ReconcileCompleted instead.
//   - `target_backend` scopes the demotion to the row owned by
//     THIS transition. Without it, a parallel `in_progress` row
//     for a different (tenant, target) pair (same tenant, other
//     target) would be incorrectly demoted by this reconcile pass.
//
// The caller passes the source-side target_backend value, which
// the worker reads from `tr.Target` for the transition being
// reconciled. The PostgreSQL parameterisation packs both source
// and target into the same query: source for the tenants join,
// target for the job-row scope.
func (s *PostgresCutoverStore) ReconcileStale(ctx context.Context, sourceBackend, targetBackend string, before, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs j
		   SET cutover_state = 'failed',
		       failure_count = j.failure_count + 1,
		       last_error    = 'stale in_progress: assumed crash recovery',
		       updated_at    = $4
		  FROM tenants t
		 WHERE j.tenant_id       = t.id
		   AND j.target_backend  = $2
		   AND j.cutover_state   = 'in_progress'
		   AND j.updated_at      < $3
		   AND t.search_backend  = $1
	`, sourceBackend, targetBackend, before, now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
