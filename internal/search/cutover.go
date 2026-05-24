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
	// to be considered eligible — only Meilisearch tenants get
	// promoted to OpenSearch.
	SourceBackend string
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
	// excluding rows that are already `in_progress` or `completed`.
	ListCandidates(ctx context.Context, f CandidateFilter) ([]string, error)
	// Claim transitions a tenant's job row to `in_progress` if no
	// other worker is currently holding it. Must be atomic — the
	// caller relies on a single winner across replicas.
	Claim(ctx context.Context, tenantID string, size, threshold int64, now time.Time) (bool, error)
	// MarkCompleted moves the row to `completed` and resets the
	// failure counter.
	MarkCompleted(ctx context.Context, tenantID string, now time.Time) error
	// MarkFailed moves the row to `failed`, increments
	// `failure_count`, and records `reason`.
	MarkFailed(ctx context.Context, tenantID, reason string, now time.Time) error
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
	//   - ReconcileCompleted only handles tenants ALREADY on
	//     OpenSearch, but here the tenant is still on Meilisearch.
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
	ReconcileStale(ctx context.Context, sourceBackend string, before, now time.Time) (int64, error)
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
func (w *CutoverWorker) Tick(ctx context.Context) {
	now := w.cfg.Now()
	staleBefore := now.Add(-w.cfg.ReconcileAfter)
	// Reconcile FIRST so any stale `in_progress` row is cleared
	// before we scan for new candidates. Two complementary cases:
	//
	//   (a) ReconcileCompleted: tenant ALREADY on OpenSearch
	//       (SetBackend committed, MarkCompleted didn't). Promote
	//       the row to `completed`.
	//   (b) ReconcileStale:     tenant STILL on Meilisearch
	//       (pod crashed during ReindexTo or before SetBackend).
	//       Demote the row to `failed` so the normal back-off /
	//       retry path picks it up on a subsequent tick. Without
	//       this, the row sits in `in_progress` forever — neither
	//       ListCandidates (excludes `in_progress`) nor
	//       ReconcileCompleted (gates on `search_backend =
	//       opensearch`) recovers it.
	//
	// Reconciliation failures are logged and ignored — the main
	// cutover loop is independent of them, and a transient store
	// blip shouldn't bring the worker down.
	if n, err := w.cfg.Store.ReconcileCompleted(ctx, BackendOpenSearch, staleBefore, now); err != nil {
		w.cfg.Logger.Printf("search.cutover: reconcile completed: %v", err)
	} else if n > 0 {
		w.cfg.Logger.Printf("search.cutover: reconcile completed: promoted %d stale in_progress rows to completed", n)
	}
	if n, err := w.cfg.Store.ReconcileStale(ctx, BackendMeilisearch, staleBefore, now); err != nil {
		w.cfg.Logger.Printf("search.cutover: reconcile stale: %v", err)
	} else if n > 0 {
		w.cfg.Logger.Printf("search.cutover: reconcile stale: demoted %d crashed in_progress rows to failed", n)
	}
	ids, err := w.cfg.Store.ListCandidates(ctx, CandidateFilter{
		SourceBackend:    BackendMeilisearch,
		MaxFailures:      w.cfg.MaxFailures,
		RetryAfterBefore: now.Add(-w.cfg.MaxRetryGap),
	})
	if err != nil {
		w.cfg.Logger.Printf("search.cutover: list candidates: %v", err)
		return
	}
	for _, tenantID := range ids {
		size, err := w.cfg.Sizer.TenantMailboxSize(ctx, tenantID)
		if err != nil {
			w.cfg.Logger.Printf("search.cutover: sizer tenant=%s: %v", tenantID, err)
			continue
		}
		if size < w.cfg.Threshold {
			continue
		}
		if err := w.cutoverOne(ctx, tenantID, size); err != nil {
			w.cfg.Logger.Printf("search.cutover: tenant %s: %v", tenantID, err)
		}
	}
}

// cutoverOne runs the full cutover dance for a single tenant. The
// state machine is the load-bearing piece: every state transition
// is a separate store write so a crash mid-flight resumes from the
// last persisted state. Concurrent workers race the `Claim` call;
// the loser short-circuits and lets the winner finish.
func (w *CutoverWorker) cutoverOne(ctx context.Context, tenantID string, size int64) error {
	now := w.cfg.Now()
	claimed, err := w.cfg.Store.Claim(ctx, tenantID, size, w.cfg.Threshold, now)
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
		_ = w.cfg.Store.MarkFailed(ctx, tenantID, fmt.Sprintf("fetch messages: %v", fetchErr), w.cfg.Now())
		return fmt.Errorf("fetch messages: %w", fetchErr)
	}
	// Reindex into OpenSearch FIRST so reads keep going to
	// Meilisearch until OpenSearch is fully populated. If the
	// reindex fails for any reason — destination 502, partial
	// network failure, schema rejection — the tenant's
	// `search_backend` column is still `meilisearch`, which
	// keeps the tenant readable AND keeps it visible to
	// `ListCandidates` for the next retry. ReindexTo deletes the
	// destination index first so a half-written previous attempt
	// doesn't leave orphan documents.
	if err := w.cfg.Service.ReindexTo(ctx, tenantID, BackendOpenSearch, msgs); err != nil {
		_ = w.cfg.Store.MarkFailed(ctx, tenantID, fmt.Sprintf("reindex: %v", err), w.cfg.Now())
		return fmt.Errorf("reindex: %w", err)
	}
	// OpenSearch is now warm; atomically flip reads over. A
	// SetBackend failure here is the unfortunate-but-recoverable
	// case: the OpenSearch index is fully populated but reads
	// still go to Meilisearch. The next tick re-discovers the
	// tenant (still on `meilisearch`), the ReindexTo wipes & re-
	// fills OpenSearch (idempotent), and the SetBackend is
	// retried.
	if err := w.cfg.Service.SetBackend(ctx, tenantID, BackendOpenSearch); err != nil {
		_ = w.cfg.Store.MarkFailed(ctx, tenantID, fmt.Sprintf("set backend: %v", err), w.cfg.Now())
		return fmt.Errorf("set backend: %w", err)
	}
	// SetBackend committed; the tenant is live on OpenSearch.
	// MarkCompleted is the bookkeeping update — it must not
	// cause a re-migration if it transiently fails. Retry with
	// exponential backoff to absorb a single connection blip; if
	// the retry budget is exhausted, the next Tick's
	// ReconcileCompleted pass will promote the row (the tenant
	// is already on OpenSearch, so the reconcile guard fires).
	if err := w.markCompletedWithRetry(ctx, tenantID); err != nil {
		w.cfg.Logger.Printf("search.cutover: tenant=%s SetBackend OK but MarkCompleted persistently failed; will be reconciled on next tick: %v", tenantID, err)
		return fmt.Errorf("mark completed: %w", err)
	}
	w.cfg.Logger.Printf("search.cutover: tenant=%s migrated %d messages to OpenSearch", tenantID, len(msgs))
	return nil
}

// markCompletedWithRetry retries MarkCompleted with exponential
// backoff up to cfg.MarkCompletedRetries times. Ctx-cancellation
// short-circuits the loop immediately so a pod shutdown isn't
// delayed by the backoff.
func (w *CutoverWorker) markCompletedWithRetry(ctx context.Context, tenantID string) error {
	var lastErr error
	for attempt := 0; attempt < w.cfg.MarkCompletedRetries; attempt++ {
		if err := w.cfg.Store.MarkCompleted(ctx, tenantID, w.cfg.Now()); err == nil {
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

// ListCandidates implements CutoverStore.
func (s *PostgresCutoverStore) ListCandidates(ctx context.Context, f CandidateFilter) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text
		FROM tenants t
		LEFT JOIN search_cutover_jobs j ON j.tenant_id = t.id
		WHERE t.search_backend = $1
		  AND (
		      j.tenant_id IS NULL
		      OR (j.cutover_state = 'failed'
		          AND j.failure_count < $2
		          AND j.updated_at < $3)
		  )
	`, f.SourceBackend, f.MaxFailures, f.RetryAfterBefore)
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
// tenant land on a single winner.
func (s *PostgresCutoverStore) Claim(ctx context.Context, tenantID string, size, threshold int64, now time.Time) (bool, error) {
	var claimed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO search_cutover_jobs (tenant_id, cutover_state, mailbox_size, threshold, started_at, updated_at)
			VALUES ($1, 'pending', $2, $3, NULL, $4)
			ON CONFLICT (tenant_id) DO NOTHING
		`, tenantID, size, threshold, now)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE search_cutover_jobs
			   SET cutover_state = 'in_progress',
			       mailbox_size  = $2,
			       started_at    = $3,
			       updated_at    = $3
			 WHERE tenant_id = $1
			   AND cutover_state IN ('pending', 'failed')
		`, tenantID, size, now)
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

// MarkCompleted implements CutoverStore.
func (s *PostgresCutoverStore) MarkCompleted(ctx context.Context, tenantID string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs
		   SET cutover_state = 'completed',
		       completed_at  = $2,
		       updated_at    = $2,
		       failure_count = 0,
		       last_error    = ''
		 WHERE tenant_id = $1
	`, tenantID, now)
	return err
}

// MarkFailed implements CutoverStore.
func (s *PostgresCutoverStore) MarkFailed(ctx context.Context, tenantID, reason string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs
		   SET cutover_state = 'failed',
		       failure_count = failure_count + 1,
		       last_error    = $2,
		       updated_at    = $3
		 WHERE tenant_id = $1
	`, tenantID, reason, now)
	return err
}

// ReconcileCompleted implements CutoverStore. The UPDATE joins
// against `tenants.search_backend` so the promotion is gated on
// the actual production state: only rows whose tenant is ALREADY
// on `targetBackend` get promoted. `updated_at` is bumped to
// `now` (caller-provided via `cfg.Now()`) so the dashboard
// reflects the recovery and integration tests can drive a
// deterministic clock.
func (s *PostgresCutoverStore) ReconcileCompleted(ctx context.Context, targetBackend string, before, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs j
		   SET cutover_state = 'completed',
		       completed_at  = $3,
		       updated_at    = $3,
		       failure_count = 0,
		       last_error    = ''
		  FROM tenants t
		 WHERE j.tenant_id      = t.id
		   AND j.cutover_state  = 'in_progress'
		   AND j.updated_at     < $2
		   AND t.search_backend = $1
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
// on the next tick. The `updated_at < $2` predicate gates by the
// reconciliation horizon so an in-flight migration on another
// worker isn't incorrectly demoted. The `tenants.search_backend =
// $1` join guarantees we only touch tenants still on the source
// backend — a tenant on `targetBackend` would be handled by
// ReconcileCompleted instead.
func (s *PostgresCutoverStore) ReconcileStale(ctx context.Context, sourceBackend string, before, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs j
		   SET cutover_state = 'failed',
		       failure_count = j.failure_count + 1,
		       last_error    = 'stale in_progress: assumed crash recovery',
		       updated_at    = $3
		  FROM tenants t
		 WHERE j.tenant_id      = t.id
		   AND j.cutover_state  = 'in_progress'
		   AND j.updated_at     < $2
		   AND t.search_backend = $1
	`, sourceBackend, before, now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
